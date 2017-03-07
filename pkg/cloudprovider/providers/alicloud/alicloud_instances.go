/*
Copyright 2016 The Kubernetes Authors All rights reserved.
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package alicloud

import (
	"github.com/denverdino/aliyungo/common"
	"github.com/denverdino/aliyungo/ecs"
	"github.com/golang/glog"
	"k8s.io/kubernetes/pkg/api/v1"
	"k8s.io/kubernetes/pkg/cloudprovider"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/runtime"
	"time"
	"strings"
	"fmt"
	"sync"
)

type SDKClientINS struct {
	regions  []ecs.RegionType
	c        *ecs.Client
	Instance map[string]*ecs.InstanceAttributesType
	NodeName types.NodeName
	lock     *sync.RWMutex
}

func NewSDKClientINS(access_key_id string, access_key_secret string) *SDKClientINS {
	ins := &SDKClientINS{
		c:        ecs.NewClient(access_key_id, access_key_secret),
		Instance: make(map[string]*ecs.InstanceAttributesType),
		lock:     new(sync.RWMutex),
	}
	regions,err := ins.c.DescribeRegions()
	if err == nil {
		ins.regions = regions
	}
	go Forever(ins,5 * time.Minute)
	return ins
}

func Forever(ins *SDKClientINS, period time.Duration) {

	for {
		func() {
			defer runtime.HandleCrash()
			glog.Infoln("Alicloud: refresh instances.")
			func (ins *SDKClientINS){
				ins.lock.Lock()
				defer ins.lock.Unlock()
				regions,err := ins.c.DescribeRegions()
				if err != nil {
					ins.regions = regions
				}
			}(ins)

			for k,v := range ins.Instance{
				if v == nil {
					continue
				}
				tmp := strings.Split(k,"/")
				if len(tmp) != 2{
					glog.Errorf("Alicloud Provider: Warning, refresh Instance info Error, Key=%s, Skip!\n",k)
					ins.Delete(k)
					continue
				}
				if _,err := ins.refreshInstance(types.NodeName(tmp[0]),common.Region(tmp[1])); err != nil{
					glog.Errorf("Alicloud Provider: Warning, refresh Instance info Error,error=%s, Key=%s, Skip!\n",err.Error(),k)
					continue
				}
				time.Sleep(time.Duration((1*time.Second)))
			}
		}()
		select {
		case <-time.After(period):
		}
	}
}
// getAddressesByName return an instance address slice by it's name.
func (s *SDKClientINS) findAddress(nodeName types.NodeName) ([]v1.NodeAddress, error) {

	instance, err := s.findInstanceByNodeName(nodeName)
	if err != nil {
		glog.Errorf("Error getting instance by InstanceId '%s': %v", nodeName, err)
		return nil, err
	}

	addrs := []v1.NodeAddress{}

	if len(instance.PublicIpAddress.IpAddress) > 0 {
		for _, ipaddr := range instance.PublicIpAddress.IpAddress {
			addrs = append(addrs, v1.NodeAddress{Type: v1.NodeExternalIP, Address: ipaddr})
		}
	}

	if instance.EipAddress.IpAddress != "" {
		addrs = append(addrs, v1.NodeAddress{Type: v1.NodeExternalIP, Address: instance.EipAddress.IpAddress})
	}

	if len(instance.InnerIpAddress.IpAddress) > 0 {
		for _, ipaddr := range instance.InnerIpAddress.IpAddress {
			addrs = append(addrs, v1.NodeAddress{Type: v1.NodeInternalIP, Address: ipaddr})
		}
	}

	if len(instance.VpcAttributes.PrivateIpAddress.IpAddress) > 0 {
		for _, ipaddr := range instance.VpcAttributes.PrivateIpAddress.IpAddress {
			addrs = append(addrs, v1.NodeAddress{Type: v1.NodeInternalIP, Address: ipaddr})
		}
	}

	if instance.VpcAttributes.NatIpAddress != "" {
		addrs = append(addrs, v1.NodeAddress{Type: v1.NodeInternalIP, Address: instance.VpcAttributes.NatIpAddress})
	}

	return addrs, nil
}

// Returns the instance with the specified node name
// Returns nil if it does not exist
func (s *SDKClientINS) findInstanceByNodeName(nodeName types.NodeName) (*ecs.InstanceAttributesType, error) {
	glog.V(2).Infof("Alicloud.findInstanceByNodeName(\"%s\")", nodeName)
	for _,region := range s.regions {
		v,_ := s.doFindInstance(nodeName,region.RegionId)
		if v != nil {
			return v,nil
		}
	}
	return nil, cloudprovider.InstanceNotFound
}

// Returns the instance with the specified node name
// Returns nil if it does not exist
func (s *SDKClientINS) findInstanceByInstanceID(insID string) (*ecs.InstanceAttributesType, error) {
	glog.V(2).Infof("Alicloud.findInstanceByInstanceID(\"%s\")", insID)
	for _,v := range s.Instance{
		if v == nil {
			continue
		}
		//glog.Infof("Range s.Instance: %s, %s\n",v.InstanceId,insID)
		if v.InstanceId == insID{
			return v, nil
		}
	}

	for _,region := range s.regions {
		v,err := s.refreshInstanceByID(insID,region.RegionId)
		if err != nil {
			glog.Warningf("findInstanceByInstanceID: cache search failed, fallback to findInstanceByInstanceID," +
				" [%s],[%s]\n",insID,region.RegionId,err.Error())
			continue
		}
		return v,nil
	}

	return nil, cloudprovider.InstanceNotFound
}

func (s *SDKClientINS) listRegions() map[string] *ecs.InstanceAttributesType{
	regs := make(map[string]*ecs.InstanceAttributesType)
	for _,v := range s.Instance{
		if v == nil {
			continue
		}
		vpcid := v.VpcAttributes.VpcId
		if _,ok := regs[vpcid];!ok{
			regs[vpcid] = v
		}
	}
	return regs
}

func (s *SDKClientINS) doFindInstance(nodeName types.NodeName, region common.Region) (*ecs.InstanceAttributesType, error){
	if v, exist := s.Get(fmt.Sprintf("%s/%s",nodeName,region));exist {
		if v != nil {
			return v, nil
		}
		// if exist && v == nil then look for another region skip s.refreshInstance().
		// nil indicate that the instance is not in this region.
	}else{
		// key not exist indicate this is the first lookup. need to refreshInstance.
		v, err := s.refreshInstance(nodeName, region);
		if err != nil{
			return nil, err
		}
		return v,nil
	}
	return nil, cloudprovider.InstanceNotFound
}

func (s *SDKClientINS) refreshInstance(nodeName types.NodeName, region common.Region) (*ecs.InstanceAttributesType, error) {
	args := ecs.DescribeInstancesArgs{
		RegionId:     region,
		InstanceName: string(nodeName),
	}
	s.NodeName = nodeName
	instances, _, err := s.c.DescribeInstances(&args)
	if err != nil {
		glog.Errorf("refreshInstance: DescribeInstances error=%s, region=%s, instanceName=%s", err.Error(),args.RegionId,args.InstanceName)
		//s.Set(fmt.Sprintf("%s/%s",nodeName,region),nil)
		return nil, err
	}

	if len(instances) == 0 {
		glog.Infof("refreshInstance: InstanceNotFound, [%s/%s]",nodeName,region)
		s.Set(fmt.Sprintf("%s/%s",nodeName,region),nil)
		return nil, cloudprovider.InstanceNotFound
	}
	if len(instances) > 1 {
		glog.Errorf("Warning: Multipul instance found by nodename [%s], the first one will be used. Instance: [%+v]", string(nodeName), instances)
	}
	glog.V(2).Infof("Alicloud.refreshInstance(\"%s\") finished. [ %+v ]\n", string(nodeName), instances[0])
	s.Set(fmt.Sprintf("%s/%s",nodeName,region),&instances[0])
	return &instances[0], nil
}

func (s *SDKClientINS) refreshInstanceByID(insid string, region common.Region) (*ecs.InstanceAttributesType, error) {
	args := ecs.DescribeInstancesArgs{
		RegionId:     region,
		InstanceIds:  fmt.Sprintf("[\"%s\"]",string(insid)),
	}
	instances, _, err := s.c.DescribeInstances(&args)
	if err != nil {
		glog.Errorf("refreshInstanceByID: DescribeInstances error=%s, region=%s, instanceName=%s", err.Error(),args.RegionId,args.InstanceName)
		//s.Set(fmt.Sprintf("%s/%s",nodeName,region),nil)
		return nil, err
	}

	if len(instances) == 0 {
		glog.Infof("refreshInstanceByID: InstanceNotFound, [%s/%s]", insid,region)
		s.Set(fmt.Sprintf("%s/%s", insid,region),nil)
		return nil, cloudprovider.InstanceNotFound
	}
	if len(instances) > 1 {
		glog.Errorf("Warning: Multipul instance found by nodename [%s], the first one will be used. Instance: [%+v]", string(insid), instances)
	}
	glog.V(2).Infof("Alicloud.refreshInstanceByID(\"%s\") finished. [ %+v ]\n", string(insid), instances[0])
	return &instances[0], nil
}



func (s *SDKClientINS) Set(k string, v *ecs.InstanceAttributesType) {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.Instance[k] = v
}

func (s *SDKClientINS) Delete(k string) {
	s.lock.Lock()
	defer s.lock.Unlock()
	delete(s.Instance,k)
}

func (s *SDKClientINS) Get(k string) (*ecs.InstanceAttributesType,bool) {
	s.lock.RLock()
	defer s.lock.RUnlock()

	v,exist := s.Instance[k]
	return v,exist
}