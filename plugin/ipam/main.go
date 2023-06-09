// Copyright 2017 CNI authors
// Copyright 2017 Lyft, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// This is a sample chained plugin that supports multiple CNI versions. It
// parses prevResult according to the cniVersion
package main

import (
	"encoding/json"
	"fmt"
	"net"
	"runtime"
	"time"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/cni/pkg/types/current"
	"github.com/containernetworking/cni/pkg/version"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/vishvananda/netlink"

	"github.com/lyft/cni-ipvlan-vpc-k8s/aws"
	"github.com/lyft/cni-ipvlan-vpc-k8s/lib"
	"github.com/lyft/cni-ipvlan-vpc-k8s/nl"
)

// PluginConf contains configuration parameters
type PluginConf struct {
	Name             string            `json:"name"`
	CNIVersion       string            `json:"cniVersion"`
	SecGroupIds      []string          `json:"secGroupIds"`
	SubnetTags       map[string]string `json:"subnetTags"`
	IfaceIndex       int               `json:"interfaceIndex"`
	SkipDeallocation bool              `json:"skipDeallocation"`
	RouteToVPCPeers  bool              `json:"routeToVpcPeers"`
	ReuseIPWait      int               `json:"reuseIPWait"`
	IPBatchSize      int64             `json:"ipBatchSize"`
	RouteToCidrs     []string          `json:"routeToCidrs"`
}

func init() {
	// this ensures that main runs only on main thread (thread group leader).
	// since namespace ops (unshare, setns) are done for a single thread, we
	// must ensure that the goroutine does not jump from OS thread to thread
	runtime.LockOSThread()
}

// parseConfig parses the supplied configuration from stdin.
func parseConfig(stdin []byte) (*PluginConf, error) {
	conf := PluginConf{
		ReuseIPWait: 60, // default 60 second wait
		IPBatchSize: 1,  // default 1 (backward compatibility)
	}

	if err := json.Unmarshal(stdin, &conf); err != nil {
		return nil, fmt.Errorf("failed to parse network configuration: %v", err)
	}

	if conf.SecGroupIds == nil {
		return nil, fmt.Errorf("secGroupIds must be specified")
	}

	return &conf, nil
}

// cmdAdd is called for ADD requests
func cmdAdd(args *skel.CmdArgs) error {
	conf, err := parseConfig(args.StdinData)
	if err != nil {
		return err
	}

	var alloc *aws.AllocationResult
	registry := &aws.Registry{}

	// Try to find a free IP first - possibly from a broken
	// container, or torn down namespace. IP must also be at least
	// conf.ReuseIPWait seconds old in the registry to be
	// considered for use.
	free, err := aws.FindFreeIPsAtIndex(conf.IfaceIndex, true)
	if err == nil || len(free) > 0 {
		registryFreeIPs, err := registry.TrackedBefore(time.Now().Add(time.Duration(-conf.ReuseIPWait) * time.Second))
		if err == nil && len(registryFreeIPs) > 0 {
		loop:
			for _, freeAlloc := range free {
				for _, freeRegistry := range registryFreeIPs {
					if freeAlloc.IP.Equal(freeRegistry) {
						alloc = freeAlloc
						// update timestamp
						err := registry.TrackIP(freeRegistry)
						if err != nil {
							return fmt.Errorf("failed to track ip: %s", err)
						}
						break loop
					}
				}
			}
		}
	}

	// No free IPs available for use, so let's allocate one
	if alloc == nil {
		// allocate IPs on an available interface
		allocs, err := aws.DefaultClient.AllocateIPsFirstAvailableAtIndex(conf.IfaceIndex, conf.IPBatchSize)
		if err == nil || len(allocs) > 0 {
			alloc = allocs[0]
		} else {
			// failed, so attempt to add an IP to a new interface
			newIf, err := aws.DefaultClient.NewInterface(conf.SecGroupIds, conf.SubnetTags, conf.IPBatchSize)
			if err != nil || len(newIf.IPv4s) < 1 {
				return fmt.Errorf("unable to create a new elastic network interface due to %v",
					err)
			}
			// Freshly allocated interfaces will always have at least one valid IP - use
			// this IP address.
			alloc = &aws.AllocationResult{
				IP:        &newIf.IPv4s[0],
				Interface: *newIf,
			}
		}
	}

	// Per https://docs.aws.amazon.com/AmazonVPC/latest/UserGuide/VPC_Subnets.html
	// subnet + 1 is our gateway
	// primary cidr + 2 is the dns server
	subnetAddr := alloc.Interface.SubnetCidr.IP.To4()
	gw := append(subnetAddr[:3], subnetAddr[3]+1)
	vpcPrimaryAddr := alloc.Interface.VpcPrimaryCidr.IP.To4()
	dns := append(vpcPrimaryAddr[:3], vpcPrimaryAddr[3]+2)
	addr := net.IPNet{
		IP:   *alloc.IP,
		Mask: alloc.Interface.SubnetCidr.Mask,
	}

	master := alloc.Interface.LocalName()

	iface := &current.Interface{
		Name: master,
	}

	// Ensure the master interface is always up
	err = nl.UpInterfacePoll(master)
	if err != nil {
		return fmt.Errorf("unable to bring up interface %v due to %v",
			master, err)
	}

	ipconfig := &current.IPConfig{
		Version:   "4",
		Address:   addr,
		Gateway:   gw,
		Interface: current.Int(0),
	}

	result := &current.Result{}
	rDNS := types.DNS{}
	rDNS.Nameservers = append(rDNS.Nameservers, dns.String())
	result.DNS = rDNS
	result.IPs = append(result.IPs, ipconfig)
	result.Interfaces = append(result.Interfaces, iface)

	cidrs := alloc.Interface.VpcCidrs
	if aws.HasBugBrokenVPCCidrs(aws.DefaultClient) {
		cidrs, err = aws.DefaultClient.DescribeVPCCIDRs(alloc.Interface.VpcID)
		if err != nil {
			return fmt.Errorf("Unable to enumerate CIDRs from the AWS API due to a specific meta-data bug %v", err)
		}
	}

	if conf.RouteToVPCPeers {
		peerCidr, err := aws.DefaultClient.DescribeVPCPeerCIDRs(alloc.Interface.VpcID)
		if err != nil {
			return fmt.Errorf("unable to enumerate peer CIDrs %v", err)
		}
		cidrs = append(cidrs, peerCidr...)
	}

	if conf.RouteToCidrs != nil {
		for _, cidr := range conf.RouteToCidrs {
			_, parsed, err := net.ParseCIDR(cidr)
			if err != nil {
				return fmt.Errorf("unable to parse routeToCidrs element %v", err)
			}
			cidrs = append(cidrs, parsed)
		}
	}

	// add routes for all VPC cidrs via the subnet gateway
	for _, dst := range cidrs {
		result.Routes = append(result.Routes, &types.Route{Dst: *dst, GW: gw})
	}

	// remove the IP from the registry just before handing off to ipvlan
	err = registry.ForgetIP(*alloc.IP)
	if err != nil {
		return fmt.Errorf("failed to forget ip: %s", err)
	}

	return types.PrintResult(result, conf.CNIVersion)
}

// cmdDel is called for DELETE requests
func cmdDel(args *skel.CmdArgs) error {
	conf, err := parseConfig(args.StdinData)
	if err != nil {
		return err
	}
	_ = conf

	var addrs []netlink.Addr

	// enter the namespace to grab the list of IPs
	_ = ns.WithNetNSPath(args.Netns, func(_ ns.NetNS) error {
		iface, err := netlink.LinkByName(args.IfName)
		if err != nil {
			return err
		}
		addrs, err = netlink.AddrList(iface, netlink.FAMILY_V4)
		return err
	})

	registry := &aws.Registry{}
	for _, addr := range addrs {
		if !conf.SkipDeallocation {
			// deallocate IPs outside of the namespace so creds are correct
			err := aws.DefaultClient.DeallocateIP(&addr.IP)
			if err != nil {
				return fmt.Errorf("failed to deallocate ip: %s", err)
			}
		}
		// Mark this IP as free in the registry
		err := registry.TrackIP(addr.IP)
		if err != nil {
			return fmt.Errorf("failed to track ip: %s", err)
		}
	}

	return nil
}

// cmdCheck is called for CHECK requests
func cmdCheck(args *skel.CmdArgs) error {
	// TODO: implement this
	return nil
}

func main() {
	run := func() error {
		skel.PluginMain(cmdAdd, cmdCheck, cmdDel, version.PluginSupports(version.Current()), "ipam")
		return nil
	}
	_ = lib.LockfileRun(run)
}
