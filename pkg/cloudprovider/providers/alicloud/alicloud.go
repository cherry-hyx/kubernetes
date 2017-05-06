/*
Copyright 2014 The Kubernetes Authors.

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
	"encoding/json"
	"errors"
	"fmt"
	"github.com/denverdino/aliyungo/common"
	"github.com/denverdino/aliyungo/metadata"
	"github.com/denverdino/aliyungo/slb"
	"github.com/golang/glog"
	"io"
	"k8s.io/kubernetes/pkg/api/v1"
	"k8s.io/kubernetes/pkg/cloudprovider"
	"k8s.io/apimachinery/pkg/types"
	"os"
	"strconv"
	"strings"
)

// ProviderName is the name of this cloud provider.
const ProviderName = "alicloud"

// Cloud is an implementation of Interface, LoadBalancer and Instances for Alicloud Services.
type Cloud struct {
	meta *metadata.MetaData
	slb  *SDKClientSLB
	ins  *SDKClientINS

	routes * SDKClientRoutes

	cfg    *CloudConfig
	region common.Region
	vpcID  string
}

var (
	DEFAULT_CHARGE_TYPE  = common.PayByTraffic
	DEFAULT_BANDWIDTH    = 50
	DEFAULT_ADDRESS_TYPE = slb.InternetAddressType

	// DEFAULT_REGION should be override in cloud initialize.
	DEFAULT_REGION	     = common.Hangzhou
)

// CloudConfig wraps the settings for the AWS cloud provider.
type CloudConfig struct {
	Global struct {
		KubernetesClusterTag string

		AccessKeyID     string `json:"accessKeyID"`
		AccessKeySecret string `json:"accessKeySecret"`
		Region          string `json:"region"`
	}
}

func init() {
	cloudprovider.RegisterCloudProvider(ProviderName,
		func(config io.Reader) (cloudprovider.Interface, error) {
			if config == nil {
				return nil, errors.New("Alicloud: config must be provided!")
			}
			var cfg CloudConfig
			if err := json.NewDecoder(config).Decode(&cfg); err != nil {
				return nil, err
			}
			if cfg.Global.AccessKeyID == "" || cfg.Global.AccessKeySecret == "" {
				return nil, errors.New("Alicloud: Provider AccessKeyID and AccessKeySecret must be provided!")
			}
			return newAliCloud(&cfg)
		})
}

func newAliCloud(config *CloudConfig) (*Cloud, error) {
	c := &Cloud{
		meta: metadata.NewMetaData(nil),
	}
	if config.Global.Region != "" {
		c.region = common.Region(config.Global.Region)
	} else {
		defer func() {
			if err := recover(); err != nil {
				fmt.Println(err)
			}
		}()
		// if region not configed ,try to detect. return err if failed. this will work with vpc network
		r, err := c.meta.Region()
		if err != nil {
			return nil, errors.New("Please provide region in Alicloud configuration file or make sure your ECS is under VPC network.")
		}
		c.region = common.Region(r)

		v, err := c.meta.VpcID()
		if err != nil {
			return nil, errors.New(fmt.Sprintf("Alicloud: error get vpcid. %s\n",err.Error()))
		}
		c.vpcID = v

		glog.Infof("Using vpc region:[region: %s],[vpcid: %s]", r,c.vpcID)
	}
	DEFAULT_REGION = c.region
	c.slb = NewSDKClientSLB(config.Global.AccessKeyID, config.Global.AccessKeySecret)
	c.ins = NewSDKClientINS(config.Global.AccessKeyID, config.Global.AccessKeySecret)
	r, err := NewSDKClientRoutes(config.Global.AccessKeyID, config.Global.AccessKeySecret);
	if err != nil{
		glog.V(2).Infof("Alicloud: error create routesdk, [%s]\n",err.Error())
		return c,nil
	}
	c.routes = r
	return c, nil
}

// TODO: Break this up into different interfaces (LB, etc) when we have more than one type of service
// GetLoadBalancer returns whether the specified load balancer exists, and
// if so, what its status is.
// Implementations must treat the *v1.Service parameter as read-only and not modify it.
// Parameter 'clusterName' is the name of the cluster as presented to kube-controller-manager
func (c *Cloud) GetLoadBalancer(clusterName string, service *v1.Service) (status *v1.LoadBalancerStatus, exists bool, err error) {

	lb, exists, err := c.slb.GetLoadBalancerByName(
		cloudprovider.GetLoadBalancerName(service),service)

	if err != nil || !exists {
		return nil, exists, err
	}

	return &v1.LoadBalancerStatus{
		Ingress: []v1.LoadBalancerIngress{{IP: lb.Address}},
	}, true, nil
}

// EnsureLoadBalancer creates a new load balancer 'name', or updates the existing one. Returns the status of the balancer
// Implementations must treat the *v1.Service and *v1.Node
// parameters as read-only and not modify them.
// Parameter 'clusterName' is the name of the cluster as presented to kube-controller-manager
func (c *Cloud) EnsureLoadBalancer(clusterName string, service *v1.Service, nodes []*v1.Node) (*v1.LoadBalancerStatus, error) {
	ns := c.filterOutByRegion(nodes, ExtractAnnotationRequest(service).Region)
	annotations := service.Annotations
	glog.V(2).Infof("alicloud.EnsureLoadBalancer(%v, %v, %v, %v, %v, %v, %v, %v,%v)",
		clusterName, service.Namespace, service.Name, c.region, service.Spec.LoadBalancerIP, service.Spec.Ports, nodes, annotations,ns)
	if service.Spec.SessionAffinity != v1.ServiceAffinityNone {
		// Does not support SessionAffinity
		return nil, fmt.Errorf("unsupported load balancer affinity: %v", service.Spec.SessionAffinity)
	}
	if len(service.Spec.Ports) == 0 {
		return nil, fmt.Errorf("requested load balancer with no ports")
	}
	if service.Spec.LoadBalancerIP != "" {
		return nil, fmt.Errorf("LoadBalancerIP cannot be specified for AWS ELB")
	}
	lb, err := c.slb.EnsureLoadBalancer(service, ns)
	if err != nil {
		return nil, err
	}

	return &v1.LoadBalancerStatus{
		Ingress: []v1.LoadBalancerIngress{{IP: lb.Address}},
	}, nil
}

// UpdateLoadBalancer updates hosts under the specified load balancer.
// Implementations must treat the *v1.Service and *v1.Node
// parameters as read-only and not modify them.
// Parameter 'clusterName' is the name of the cluster as presented to kube-controller-manager
func (c *Cloud) UpdateLoadBalancer(clusterName string, service *v1.Service, nodes []*v1.Node) error {
	glog.V(2).Infof("alicloud.UpdateLoadBalancer(%v, %v, %v, %v, %v, %v, %v)",
		clusterName, service.Namespace, service.Name, c.region, service.Spec.LoadBalancerIP, service.Spec.Ports, nodes)

	return c.slb.UpdateLoadBalancer(service, c.filterOutByRegion(nodes, ExtractAnnotationRequest(service).Region))
}

// EnsureLoadBalancerDeleted deletes the specified load balancer if it
// exists, returning nil if the load balancer specified either didn't exist or
// was successfully deleted.
// This construction is useful because many cloud providers' load balancers
// have multiple underlying components, meaning a Get could say that the LB
// doesn't exist even if some part of it is still laying around.
// Implementations must treat the *v1.Service parameter as read-only and not modify it.
// Parameter 'clusterName' is the name of the cluster as presented to kube-controller-manager
func (c *Cloud) EnsureLoadBalancerDeleted(clusterName string, service *v1.Service) error {
	glog.V(2).Infof("alicloud.EnsureLoadBalancerDeleted(%v, %v, %v, %v, %v, %v)",
		clusterName, service.Namespace, service.Name, c.region, service.Spec.LoadBalancerIP, service.Spec.Ports)
	return c.slb.EnsureLoadBalanceDeleted(service)
}

// NodeAddresses returns the addresses of the specified instance.
// TODO(roberthbailey): This currently is only used in such a way that it
// returns the address of the calling instance. We should do a rename to
// make this clearer.
func (c *Cloud) NodeAddresses(name types.NodeName) ([]v1.NodeAddress, error) {
	glog.V(2).Infof("Alicloud.NodeAddresses(\"%s\")", string(name))
	return c.ins.findAddress(name)
}

// ExternalID returns the cloud provider ID of the node with the specified NodeName.
// Note that if the instance does not exist or is no longer running, we must return ("", cloudprovider.InstanceNotFound)
func (c *Cloud) ExternalID(nodeName types.NodeName) (string, error) {
	glog.V(2).Infof("Alicloud.ExternalID(\"%s\")", string(nodeName))
	instance, err := c.ins.findInstanceByNodeName(nodeName)
	if err != nil {
		return "", err
	}
	return instance.InstanceId, nil
}

// InstanceID returns the cloud provider ID of the node with the specified NodeName.
func (c *Cloud) InstanceID(nodeName types.NodeName) (string, error) {
	glog.V(2).Infof("Alicloud.InstanceID(\"%s\")", string(nodeName))
	instance, err := c.ins.findInstanceByNodeName(nodeName)
	if err != nil {
		return "", err
	}
	return instance.InstanceId, nil
}

// InstanceType returns the type of the specified instance.
func (c *Cloud) InstanceType(name types.NodeName) (string, error) {
	instance, err := c.ins.findInstanceByNodeName(name)
	if err != nil {
		return "", err
	}
	return instance.InstanceType, nil
}

// AddSSHKeyToAllInstances adds an SSH public key as a legal identity for all instances
// expected format for the key is standard ssh-keygen format: <protocol> <blob>
func (c *Cloud) AddSSHKeyToAllInstances(user string, keyData []byte) error {
	return errors.New("Alicloud.AddSSHKeyToAllInstances not implemented")
}

// CurrentNodeName returns the name of the node we are currently running on
// On most clouds (e.g. GCE) this is the hostname, so we provide the hostname
func (c *Cloud) CurrentNodeName(hostname string) (types.NodeName, error) {
	glog.V(2).Infof("Alicloud.CurrentNodeName(\"%s\")", hostname)
	return types.NodeName(hostname), nil
}

// ListRoutes lists all managed routes that belong to the specified clusterName
func (c *Cloud) ListRoutes(clusterName string) ([]*cloudprovider.Route, error) {
	routes := []*cloudprovider.Route{}
	for _,v := range c.ins.listRegions(){
		r, err := c.routes.ListRoutes(v)
		if err != nil {
			fmt.Errorf("Alicloud.ListRoutes : ERROR , %s\n",err.Error())
			return nil, err
		}
		routes = append(routes, r...)
	}
	for k,v := range routes{
		ins, err := c.ins.findInstanceByInstanceID(v.Name)
		if err != nil {
			glog.Warningf("Alicloud.ListRoutes(%s): cant find instanceid [%s].\n",v.Name,err.Error())
			continue
		}
		// Fix route name
		routes[k].TargetNode=types.NodeName(strings.ToLower(ins.InstanceName))
		glog.V(2).Infof("Alicloud.ListRoutes(): route[%d]=> %v",k,v)
	}
	return routes,nil
}

// CreateRoute creates the described managed route
// route.Name will be ignored, although the cloud-provider may use nameHint
// to create a more user-meaningful name.
func (c *Cloud) CreateRoute(clusterName string, nameHint string, route *cloudprovider.Route) error {
	glog.V(2).Infof("Alicloud.CreateRoute(\"%s, %v\")", clusterName, route)
	ins, err := c.ins.findInstanceByNodeName(route.TargetNode)
	if err != nil {
		return err
	}
	cRoute := &cloudprovider.Route{
		Name: route.Name,
		DestinationCIDR: route.DestinationCIDR,
		TargetNode: types.NodeName(ins.InstanceId),
	}
	return c.routes.CreateRoute(cRoute,ins)
}

// DeleteRoute deletes the specified managed route
// Route should be as returned by ListRoutes
func (c *Cloud) DeleteRoute(clusterName string, route *cloudprovider.Route) error {
	glog.V(2).Infof("Alicloud.DeleteRoute(\"%s, %v\")",clusterName, route)
	ins, err := c.ins.findInstanceByNodeName(route.TargetNode)
	if err != nil {
		return err
	}
	cRoute := &cloudprovider.Route{
		Name: route.Name,
		DestinationCIDR: route.DestinationCIDR,
		TargetNode: types.NodeName(ins.InstanceId),
	}
	return c.routes.DeleteRoute(cRoute, ins)
}

// GetZone returns the Zone containing the current failure zone and locality region that the program is running in
func (c *Cloud) GetZone() (cloudprovider.Zone, error) {
	host, err := os.Hostname()
	if err != nil {
		return cloudprovider.Zone{}, errors.New(fmt.Sprintf("Alicloud.GetZone(), error os.Hostname() %s", err.Error()))
	}
	i, err := c.ins.findInstanceByNodeName(types.NodeName(host))
	if err != nil {
		return cloudprovider.Zone{}, errors.New(fmt.Sprintf("Alicloud.GetZone(), error findInstanceByNodeName() %s", err.Error()))
	}
	return cloudprovider.Zone{
		Region:        string(c.region),
		FailureDomain: i.ZoneId,
	}, nil
}

// ListClusters lists the names of the available clusters.
func (c *Cloud) ListClusters() ([]string, error) {
	return nil, errors.New("Alicloud.ListClusters not implemented")
}

// Master gets back the address (either DNS name or IP address) of the master node for the cluster.
func (c *Cloud) Master(clusterName string) (string, error) {
	return "", errors.New("Alicloud.ListClusters not implemented")
}

// Clusters returns the list of clusters.
func (c *Cloud) Clusters() (cloudprovider.Clusters, bool) {
	return nil, false
}

// ProviderName returns the cloud provider ID.
func (c *Cloud) ProviderName() string {
	return ProviderName
}

// ScrubDNS filters DNS settings for pods.
func (c *Cloud) ScrubDNS(nameservers, searches []string) (nsOut, srchOut []string) {
	return nameservers, searches
}

// LoadBalancer returns an implementation of LoadBalancer for Alicloud Services.
func (c *Cloud) LoadBalancer() (cloudprovider.LoadBalancer, bool) {
	return c, true
}

// Instances returns an implementation of Instances for Alicloud Services.
func (c *Cloud) Instances() (cloudprovider.Instances, bool) {
	return c, true
}

// Zones returns an implementation of Zones for Alicloud Services.
func (c *Cloud) Zones() (cloudprovider.Zones, bool) {
	return c, true
}

// Routes returns an implementation of Routes for Alicloud Services.
func (c *Cloud) Routes() (cloudprovider.Routes, bool) {
	if c.vpcID != "" && c.routes != nil{
		glog.V(2).Infof("Alicloud: Routes enabled!\n")
		return c, true
	}
	return nil, false
}

// filterOutByRegion Used for multi-region or multi-vpc. works for single region or vpc too.
// SLB only support Backends within the same vpc in the same region. so we need to remove the other backends which not in
// the same region vpc with teh SLB. Keep the most backends
func (c *Cloud) filterOutByRegion(nodes []*v1.Node, region common.Region) []*v1.Node{
	result := []*v1.Node{}
	mvpc := make(map[string]int)
	for _,node := range nodes{
		glog.V(2).Infof("filterOutByRegion: for node => %v\n",node.Name)
		v, err := c.ins.doFindInstance(types.NodeName(node.Name),region);
		if err != nil{
			glog.V(2).Infof("filterOutByRegion: c.ins.doFindInstance error => %s\n",err.Error())
			continue
		}
		if v != nil {

			mvpc[v.VpcAttributes.VpcId] = mvpc[v.VpcAttributes.VpcId] + 1
			//result = append(result, node)
			glog.V(2).Infof("filterOutByRegion: accept node => %v\n",node.Name)
		}
	}
	max, key := 0, ""
	for k,v := range mvpc{
		if v > max{
			max = v
			key = k
		}
	}
	for _,node := range nodes{
		glog.V(2).Infof("filterOutByRegion: for node => %v\n",node.Name)
		v, err := c.ins.doFindInstance(types.NodeName(node.Name),region);
		if err != nil{
			glog.V(2).Infof("filterOutByRegion: c.ins.doFindInstance error => %s\n",err.Error())
			continue
		}
		if v != nil && v.VpcAttributes.VpcId == key{
			result = append(result, node)
			glog.V(2).Infof("filterOutByRegion: accept node => %v\n",node.Name)
		}
	}
	return result
}

func ExtractAnnotationRequest(service *v1.Service) *AnnotationRequest {
	ar := &AnnotationRequest{}
	annotation := service.Annotations

	i, err := strconv.Atoi(annotation[ServiceAnnotationLoadBalancerBandwidth])
	if err != nil {
		glog.Errorf("Warining: Annotation bandwidth must be integer,got [%s],use default number 50.",
			annotation[ServiceAnnotationLoadBalancerBandwidth])
		ar.Bandwidth = DEFAULT_BANDWIDTH
	} else {
		ar.Bandwidth = i
	}
	addtype := annotation[ServiceAnnotationLoadBalancerAddressType]
	if addtype != "" {
		ar.AddressType = slb.AddressType(addtype)
	} else {
		ar.AddressType = slb.InternetAddressType
	}

	chargtype := annotation[ServiceAnnotationLoadBalancerChargeType]
	if chargtype != "" {
		ar.ChargeType = slb.InternetChargeType(chargtype)
	} else {
		ar.ChargeType = slb.PayByTraffic
	}

	region := annotation[ServiceAnnotationLoadBalancerRegion]
	if region != "" {
		ar.Region = common.Region(region)
	} else {
		ar.Region = DEFAULT_REGION
	}

	certid := annotation[ServiceAnnotationLoadBalancerCertID]
	if certid != "" {
		ar.CertID = certid
	}

	hcFlag := annotation[ServiceAnnotationLoadBalancerHealthCheckFlag]
	if hcFlag != "" {
		ar.HealthCheck = slb.FlagType(hcFlag)
	} else {
		ar.HealthCheck = slb.OffFlag
	}

	hcType := annotation[ServiceAnnotationLoadBalancerHealthCheckType]
	if hcType != "" {
		ar.HealthCheckType = slb.HealthCheckType(hcType)
	} else {
		ar.HealthCheckType = slb.TCPHealthCheckType
	}

	hcUri := annotation[ServiceAnnotationLoadBalancerHealthCheckURI]
	if hcUri != "" {
		ar.HealthCheckURI = hcUri
	} else {
		ar.HealthCheckURI = "/"
	}

	port, err := strconv.Atoi(annotation[ServiceAnnotationLoadBalancerHealthCheckConnectPort])
	if err != nil {
		ar.HealthCheckConnectPort = -520
	} else {
		ar.HealthCheckConnectPort = port
	}

	thresh, err := strconv.Atoi(annotation[ServiceAnnotationLoadBalancerHealthCheckHealthyThreshold])
	if err != nil {
		ar.HealthyThreshold = 3
	} else {
		ar.HealthyThreshold = thresh
	}

	unThresh, err := strconv.Atoi(annotation[ServiceAnnotationLoadBalancerHealthCheckUnhealthyThreshold])
	if err != nil {
		ar.UnhealthyThreshold = 3
	} else {
		ar.UnhealthyThreshold = unThresh
	}

	interval, err := strconv.Atoi(annotation[ServiceAnnotationLoadBalancerHealthCheckInterval])
	if err != nil {
		ar.HealthCheckInterval = 2
	} else {
		ar.HealthCheckInterval = interval
	}

	connout, err := strconv.Atoi(annotation[ServiceAnnotationLoadBalancerHealthCheckConnectTimeout])
	if err != nil {
		ar.HealthCheckConnectTimeout = 5
	} else {
		ar.HealthCheckConnectTimeout = connout
	}

	hout, err := strconv.Atoi(annotation[ServiceAnnotationLoadBalancerHealthCheckTimeout])
	if err != nil {
		ar.HealthCheckTimeout = 5
	} else {
		ar.HealthCheckConnectPort = hout
	}
	return ar
}