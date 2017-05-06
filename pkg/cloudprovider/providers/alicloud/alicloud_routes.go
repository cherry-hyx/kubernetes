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
	"k8s.io/kubernetes/pkg/cloudprovider"

	"github.com/denverdino/aliyungo/ecs"
	"sync"
	"github.com/golang/glog"
	"k8s.io/apimachinery/pkg/types"
	"strings"
	"errors"
	"fmt"
)

type SDKClientRoutes struct {
	client   *ecs.Client
	lock     *sync.RWMutex
}

func NewSDKClientRoutes(access_key_id string, access_key_secret string) (*SDKClientRoutes,error) {
	sdk := &SDKClientRoutes{
		client:        ecs.NewClient(access_key_id, access_key_secret),
	}
	return sdk,nil
}

func (r *SDKClientRoutes) getRouteTableByInstance(i *ecs.InstanceAttributesType) (string,string,error){

	vpc, _, err := r.client.DescribeVpcs(&ecs.DescribeVpcsArgs{
		RegionId: i.RegionId,
		VpcId:    i.VpcAttributes.VpcId,
	})
	if err != nil {
		glog.V(2).Infof("Alicloud: Error ecs.DescribeVpcs(%s,%s), %s . \n",i.RegionId,i.VpcAttributes.VpcId, r.getErrorString(err))
		return "","", err
	}
	if len(vpc) <= 0{
		return "","", errors.New(fmt.Sprintf("Can not find vpc Meta by instance[ins=%s][region=%s][vpcid=%s], error: len = 0",i.InstanceId,i.RegionId,i.VpcAttributes.VpcId))
	}
	vroute, _, err := r.client.DescribeVRouters(&ecs.DescribeVRoutersArgs{
		VRouterId: vpc[0].VRouterId,
		RegionId:  i.RegionId,
	})
	if err != nil {
		glog.V(2).Infof("Alicloud: Error ecs.DescribeVRouters(%s,%s), %s .\n",vpc[0].VRouterId,i.RegionId, r.getErrorString(err))
		return "", "",err
	}
	if len(vroute) <= 0{
		return "","", errors.New(fmt.Sprintf("Can not find VRouter Meta by instance[ins=%s][region=%s][vpcid=%s], error: len = 0",i.InstanceId,i.RegionId,i.VpcAttributes.VpcId))
	}

	return vroute[0].VRouterId, vroute[0].RouteTableIds.RouteTableId[0],nil
}

// ListRoutes lists all managed routes that belong to the specified clusterName
func (r *SDKClientRoutes) ListRoutes(ins *ecs.InstanceAttributesType) ([]*cloudprovider.Route, error){
	vRouteID,rTableID,err := r.getRouteTableByInstance(ins)
	rtables, _, err := r.client.DescribeRouteTables(&ecs.DescribeRouteTablesArgs{
		VRouterId:    vRouteID,
		RouteTableId: rTableID,
	})
	if err != nil {
		glog.V(2).Infof("Alicloud: Error ecs.DescribeRouteTables() %s.\n", r.getErrorString(err))
		return nil, err
	}

	tabs := []*cloudprovider.Route{}
	if len(rtables) <= 0 {
		glog.V(2).Infof("Alicloud WARINING: SDKClientRoutes.SDKClientRoutes(%s),vRouteID=%s,rTableID=%s\n",
		ins.InstanceId,vRouteID,rTableID)
		return tabs,nil
	}
	for _, e := range rtables[0].RouteEntrys.RouteEntry{
		//skip none custom route
		if e.Type != ecs.RouteTableCustom {
			continue
		}
		// skip none Instance route
		if strings.ToLower(e.NextHopType) != "instance"{
			continue
		}
		// skip DNAT route
		if e.DestinationCidrBlock == "0.0.0.0/0"{
			continue
		}
		route := &cloudprovider.Route{
			Name: e.InstanceId,
			TargetNode: types.NodeName(e.InstanceId),
			DestinationCIDR:e.DestinationCidrBlock,
		}
		tabs = append(tabs, route)
	}
	return tabs,nil
}


// CreateRoute creates the described managed route
// route.Name will be ignored, although the cloud-provider may use nameHint
// to create a more user-meaningful name.
func (r *SDKClientRoutes) CreateRoute( route *cloudprovider.Route,ins *ecs.InstanceAttributesType) error{
	vRouteID,rTableID,err := r.getRouteTableByInstance(ins)
	if err != nil {
		return err
	}
	rtables, _, err := r.client.DescribeRouteTables(&ecs.DescribeRouteTablesArgs{
		VRouterId:    vRouteID,
		RouteTableId: rTableID,
	})
	if err != nil {
		glog.V(2).Infof("Alicloud: Error ecs.DescribeRouteTables() %s.\n", err.Error())
		return err
	}
	if len(rtables) <= 0 {
		glog.V(2).Infof("Alicloud WARINING: SDKClientRoutes.SDKClientRoutes(%s),vRouteID=%s,rTableID=%s\n",
			ins.InstanceId,vRouteID,rTableID)
		return errors.New(fmt.Sprintf("SDKClientRoutes.CreateRoute(%s,%s),Error: cant find route table [vRouteID=%s,rTableID=%s]",ins.InstanceId,route.DestinationCIDR,vRouteID,rTableID))
	}
	if err := r.reCreateRoute(rtables[0], &ecs.CreateRouteEntryArgs{
		DestinationCidrBlock: route.DestinationCIDR,
		NextHopType:          ecs.NextHopIntance,
		NextHopId:            string(route.TargetNode),
		ClientToken:          "",
		RouteTableId:         rTableID,
	}); err != nil {
		return err
	}

	if err := r.client.WaitForAllRouteEntriesAvailable(vRouteID, rTableID, 60);
		err != nil {
		return err
	}
	return nil
}

// DeleteRoute deletes the specified managed route
// Route should be as returned by ListRoutes
func (r *SDKClientRoutes) DeleteRoute( route *cloudprovider.Route, ins *ecs.InstanceAttributesType) error{
	vRouteID,rTableID,err := r.getRouteTableByInstance(ins)
	if err != nil {
		return err
	}
	if err := r.client.DeleteRouteEntry(&ecs.DeleteRouteEntryArgs{
		RouteTableId:         rTableID,
		DestinationCidrBlock: route.DestinationCIDR,
		NextHopId:            string(route.TargetNode),
	}); err != nil {
		return err
	}
	if err := r.client.WaitForAllRouteEntriesAvailable(vRouteID, rTableID, 60);
		err != nil {
		return err
	}
	return nil
}


func (r *SDKClientRoutes) reCreateRoute(table ecs.RouteTableSetType, route *ecs.CreateRouteEntryArgs) error {

	exist := false
	for _, e := range table.RouteEntrys.RouteEntry {
		if e.RouteTableId == route.RouteTableId &&
			e.Type == ecs.RouteTableCustom &&
			e.InstanceId == route.NextHopId {

			if e.DestinationCidrBlock == route.DestinationCidrBlock &&
				e.Status == ecs.RouteEntryStatusAvailable {
				exist = true
				glog.V(2).Infof("Keep target entry: rtableid=%s, CIDR=%s, NextHop=%s \n", e.RouteTableId, e.DestinationCidrBlock, e.InstanceId)
				continue
			}

			// 0.0.0.0/0 => ECS1 this kind of route is used for DNAT. so we keep it
			if e.DestinationCidrBlock == "0.0.0.0/0" {
				glog.V(2).Infof("Keep route entry: rtableid=%s, CIDR=%s, NextHop=%s For DNAT\n", e.RouteTableId, e.DestinationCidrBlock, e.InstanceId)
				continue
			}
			// Fix: here we delete all the route which targeted to us(instance) except the specified route.
			// That means only one CIDR was allowed to target to the instance. Think if We need to change this
			// to adapt to multi CIDR and deal with unavailable route entry.
			if err := r.client.DeleteRouteEntry(&ecs.DeleteRouteEntryArgs{
				RouteTableId:         route.RouteTableId,
				DestinationCidrBlock: e.DestinationCidrBlock,
				NextHopId:            route.NextHopId,
			}); err != nil {
				return err
			}

			glog.V(2).Infof("Remove old route entry: rtableid=%s, CIDR=%s, NextHop=%s \n", e.RouteTableId, e.DestinationCidrBlock, e.InstanceId)
			continue
		}

		glog.V(2).Infof("Keep route entry: rtableid=%s, CIDR=%s, NextHop=%s \n", e.RouteTableId, e.DestinationCidrBlock, e.InstanceId)
	}
	if !exist {
		return r.client.CreateRouteEntry(route)
	}
	return nil
}

func (r *SDKClientRoutes) getErrorString(e error) string {
	if e == nil {
		return ""
	}
	return e.Error()
}