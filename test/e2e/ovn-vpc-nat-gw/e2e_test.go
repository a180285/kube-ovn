package ovn_eip

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	dockertypes "github.com/docker/docker/api/types"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/test/e2e"
	k8sframework "k8s.io/kubernetes/test/e2e/framework"
	"k8s.io/kubernetes/test/e2e/framework/config"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"

	"github.com/onsi/ginkgo/v2"

	kubeovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
	"github.com/kubeovn/kube-ovn/pkg/ovs"
	"github.com/kubeovn/kube-ovn/pkg/util"
	"github.com/kubeovn/kube-ovn/test/e2e/framework"
	"github.com/kubeovn/kube-ovn/test/e2e/framework/docker"
	"github.com/kubeovn/kube-ovn/test/e2e/framework/iproute"
	"github.com/kubeovn/kube-ovn/test/e2e/framework/kind"
)

const dockerNetworkName = "kube-ovn-vlan"

const dockerExtraNetworkName = "kube-ovn-extra-vlan"

func makeProviderNetwork(providerNetworkName string, exchangeLinkName bool, linkMap map[string]*iproute.Link) *kubeovnv1.ProviderNetwork {
	var defaultInterface string
	customInterfaces := make(map[string][]string, 0)
	for node, link := range linkMap {
		if !strings.ContainsRune(node, '-') {
			continue
		}

		if defaultInterface == "" {
			defaultInterface = link.IfName
		} else if link.IfName != defaultInterface {
			customInterfaces[link.IfName] = append(customInterfaces[link.IfName], node)
		}
	}

	return framework.MakeProviderNetwork(providerNetworkName, exchangeLinkName, defaultInterface, customInterfaces, nil)
}

func makeOvnEip(name, subnet, v4ip, v6ip, mac, usage string) *kubeovnv1.OvnEip {
	return framework.MakeOvnEip(name, subnet, v4ip, v6ip, mac, usage)
}

func makeOvnVip(namespaceName, name, subnet, v4ip, v6ip, vipType string) *kubeovnv1.Vip {
	return framework.MakeVip(namespaceName, name, subnet, v4ip, v6ip, vipType)
}

func makeOvnFip(name, ovnEip, ipType, ipName, vpc, v4Ip string) *kubeovnv1.OvnFip {
	return framework.MakeOvnFip(name, ovnEip, ipType, ipName, vpc, v4Ip)
}

func makeOvnSnat(name, ovnEip, vpcSubnet, ipName, vpc, v4IpCidr string) *kubeovnv1.OvnSnatRule {
	return framework.MakeOvnSnatRule(name, ovnEip, vpcSubnet, ipName, vpc, v4IpCidr)
}

func makeOvnDnat(name, ovnEip, ipType, ipName, vpc, v4Ip, internalPort, externalPort, protocol string) *kubeovnv1.OvnDnatRule {
	return framework.MakeOvnDnatRule(name, ovnEip, ipType, ipName, vpc, v4Ip, internalPort, externalPort, protocol)
}

var _ = framework.Describe("[group:ovn-vpc-nat-gw]", func() {
	f := framework.NewDefaultFramework("ovn-vpc-nat-gw")

	time.Sleep(5 * time.Second)
	var skip bool
	var itFn func(bool, string, map[string]*iproute.Link, *[]string)
	var cs clientset.Interface
	var dockerNetwork, dockerExtraNetwork *dockertypes.NetworkResource
	var nodeNames, gwNodeNames, providerBridgeIps []string
	var clusterName, providerNetworkName, vlanName, underlaySubnetName, noBfdVpcName, bfdVpcName, noBfdSubnetName, bfdSubnetName string
	var providerExtraNetworkName, vlanExtraName, underlayExtraSubnetName, noBfdExtraSubnetName string
	var linkMap, extraLinkMap map[string]*iproute.Link
	var providerNetworkClient *framework.ProviderNetworkClient
	var vlanClient *framework.VlanClient
	var vpcClient *framework.VpcClient
	var subnetClient *framework.SubnetClient
	var ovnEipClient *framework.OvnEipClient
	var vipClient *framework.VipClient
	var ovnFipClient *framework.OvnFipClient
	var ovnSnatRuleClient *framework.OvnSnatRuleClient
	var ovnDnatRuleClient *framework.OvnDnatRuleClient
	var podClient *framework.PodClient

	var aapVip1Name, aapVip2Name string
	var lrpEipSnatName, lrpExtraEipSnatName string
	var dnatVipName, dnatName string
	var fipVipName, fipEipName, fipName string
	var snatEipName, snatName string
	var ipDnatVipName, ipDnatEipName, ipDnatName string
	var ipFipVipName, ipFipEipName, ipFipName string
	var cidrSnatEipName, cidrSnatName, ipSnatVipName, ipSnatEipName, ipSnatName string

	var containerID string
	var namespaceName string

	var sharedVipName, sharedEipDnatName, sharedEipFipShoudOkName, sharedEipFipShoudFailName string
	var fipPodName, dnatEipName, podFipName string
	var fipExtraPodName, podExtraEipName, podExtraFipName string

	var nodes []kind.Node

	networkToolImage := "cr.sihe.cloud/docker.io/jonlabelle/network-tools@sha256:2f4cd61ca9ad57626b5576bf8398a09353e97d75e6c98c8b1d735301c600db8f"
	nginxImage := "cr.sihe.cloud/docker.io/nginx:1.25.5"

	ginkgo.BeforeEach(func() {
		cs = f.ClientSet
		subnetClient = f.SubnetClient()
		vlanClient = f.VlanClient()
		vpcClient = f.VpcClient()
		providerNetworkClient = f.ProviderNetworkClient()
		ovnEipClient = f.OvnEipClient()
		vipClient = f.VipClient()
		ovnFipClient = f.OvnFipClient()
		ovnSnatRuleClient = f.OvnSnatRuleClient()
		ovnDnatRuleClient = f.OvnDnatRuleClient()

		podClient = f.PodClient()

		namespaceName = f.Namespace.Name

		gwNodeNum := 2
		// gw node is 2 means e2e HA cluster will have 2 gw nodes and a worker node
		// in this env, tcpdump gw nat flows will be more clear

		noBfdVpcName = "no-bfd-vpc-" + framework.RandomSuffix()
		bfdVpcName = "bfd-vpc-" + framework.RandomSuffix()

		// test allow address pair vip
		aapVip1Name = "aap-vip1-" + framework.RandomSuffix()
		aapVip2Name = "aap-vip2-" + framework.RandomSuffix()

		// nats use ip crd name or vip crd
		fipVipName = "fip-vip-" + framework.RandomSuffix()
		fipEipName = "fip-eip-" + framework.RandomSuffix()
		fipName = "fip-" + framework.RandomSuffix()

		dnatVipName = "dnat-vip-" + framework.RandomSuffix()
		dnatEipName = "dnat-eip-" + framework.RandomSuffix()
		dnatName = "dnat-" + framework.RandomSuffix()

		snatEipName = "snat-eip-" + framework.RandomSuffix()
		snatName = "snat-" + framework.RandomSuffix()
		noBfdSubnetName = "no-bfd-subnet-" + framework.RandomSuffix()
		noBfdExtraSubnetName = "no-bfd-extra-subnet-" + framework.RandomSuffix()
		lrpEipSnatName = "lrp-eip-snat-" + framework.RandomSuffix()
		lrpExtraEipSnatName = "lrp-extra-eip-snat-" + framework.RandomSuffix()
		bfdSubnetName = "bfd-subnet-" + framework.RandomSuffix()
		providerNetworkName = "external"
		providerExtraNetworkName = "extra"
		vlanName = "vlan-" + framework.RandomSuffix()
		vlanExtraName = "vlan-extra-" + framework.RandomSuffix()
		underlaySubnetName = "external"
		underlayExtraSubnetName = "extra"

		// sharing case
		sharedVipName = "shared-vip-" + framework.RandomSuffix()
		sharedEipDnatName = "shared-eip-dnat-" + framework.RandomSuffix()
		sharedEipFipShoudOkName = "shared-eip-fip-should-ok-" + framework.RandomSuffix()
		sharedEipFipShoudFailName = "shared-eip-fip-should-fail-" + framework.RandomSuffix()

		// pod with fip
		fipPodName = "fip-pod-" + framework.RandomSuffix()
		podFipName = fipPodName

		// pod with fip for extra external subnet
		fipExtraPodName = "fip-extra-pod-" + framework.RandomSuffix()
		podExtraEipName = fipExtraPodName
		podExtraFipName = fipExtraPodName

		// fip use ip addr
		ipFipVipName = "ip-fip-vip-" + framework.RandomSuffix()
		ipFipEipName = "ip-fip-eip-" + framework.RandomSuffix()
		ipFipName = "ip-fip-" + framework.RandomSuffix()

		// dnat use ip addr
		ipDnatVipName = "ip-dnat-vip-" + framework.RandomSuffix()
		ipDnatEipName = "ip-dnat-eip-" + framework.RandomSuffix()
		ipDnatName = "ip-dnat-" + framework.RandomSuffix()

		// snat use ip cidr
		cidrSnatEipName = "cidr-snat-eip-" + framework.RandomSuffix()
		cidrSnatName = "cidr-snat-" + framework.RandomSuffix()
		ipSnatVipName = "ip-snat-vip-" + framework.RandomSuffix()
		ipSnatEipName = "ip-snat-eip-" + framework.RandomSuffix()
		ipSnatName = "ip-snat-" + framework.RandomSuffix()

		containerID = ""

		if skip {
			ginkgo.Skip("underlay spec only runs on kind clusters")
		}

		if clusterName == "" {
			ginkgo.By("Getting k8s nodes")
			k8sNodes, err := e2enode.GetReadySchedulableNodes(context.Background(), cs)
			framework.ExpectNoError(err)

			cluster, ok := kind.IsKindProvided(k8sNodes.Items[0].Spec.ProviderID)
			if !ok {
				skip = true
				ginkgo.Skip("underlay spec only runs on kind clusters")
			}
			clusterName = cluster
		}

		if dockerNetwork == nil {
			ginkgo.By("Ensuring docker network " + dockerNetworkName + " exists")
			network, err := docker.NetworkCreate(dockerNetworkName, true, true)
			framework.ExpectNoError(err, "creating docker network "+dockerNetworkName)
			dockerNetwork = network
		}

		if dockerExtraNetwork == nil {
			ginkgo.By("Ensuring extra docker network " + dockerExtraNetworkName + " exists")
			network, err := docker.NetworkCreate(dockerExtraNetworkName, true, true)
			framework.ExpectNoError(err, "creating extra docker network "+dockerExtraNetworkName)
			dockerExtraNetwork = network
		}

		ginkgo.By("Getting kind nodes")
		var err error
		nodes, err = kind.ListNodes(clusterName, "")
		framework.ExpectNoError(err, "getting nodes in kind cluster")
		framework.ExpectNotEmpty(nodes)

		ginkgo.By("Connecting nodes to the docker network")
		err = kind.NetworkConnect(dockerNetwork.ID, nodes)
		framework.ExpectNoError(err, "connecting nodes to network "+dockerNetworkName)

		ginkgo.By("Connecting nodes to the extra docker network")
		err = kind.NetworkConnect(dockerExtraNetwork.ID, nodes)
		framework.ExpectNoError(err, "connecting nodes to extra network "+dockerExtraNetworkName)

		ginkgo.By("Getting node links that belong to the docker network")
		nodes, err = kind.ListNodes(clusterName, "")
		framework.ExpectNoError(err, "getting nodes in kind cluster")

		linkMap = make(map[string]*iproute.Link, len(nodes))
		extraLinkMap = make(map[string]*iproute.Link, len(nodes))
		nodeNames = make([]string, 0, len(nodes))
		gwNodeNames = make([]string, 0, gwNodeNum)
		providerBridgeIps = make([]string, 0, len(nodes))

		// node ext gw ovn eip name is the same as node name in this scenario
		for index, node := range nodes {
			links, err := node.ListLinks()
			framework.ExpectNoError(err, "failed to list links on node %s: %v", node.Name(), err)
			for _, link := range links {
				if link.Address == node.NetworkSettings.Networks[dockerNetworkName].MacAddress {
					linkMap[node.ID] = &link
					break
				}
			}
			for _, link := range links {
				if link.Address == node.NetworkSettings.Networks[dockerExtraNetworkName].MacAddress {
					extraLinkMap[node.ID] = &link
					break
				}
			}
			framework.ExpectHaveKey(linkMap, node.ID)
			framework.ExpectHaveKey(extraLinkMap, node.ID)
			linkMap[node.Name()] = linkMap[node.ID]
			extraLinkMap[node.Name()] = extraLinkMap[node.ID]
			nodeNames = append(nodeNames, node.Name())
			if index < gwNodeNum {
				gwNodeNames = append(gwNodeNames, node.Name())
			}
		}

		itFn = func(exchangeLinkName bool, providerNetworkName string, linkMap map[string]*iproute.Link, bridgeIps *[]string) {
			ginkgo.By("Creating provider network " + providerNetworkName)
			pn := makeProviderNetwork(providerNetworkName, exchangeLinkName, linkMap)
			pn = providerNetworkClient.CreateSync(pn)

			ginkgo.By("Getting k8s nodes")
			k8sNodes, err := e2enode.GetReadySchedulableNodes(context.Background(), cs)
			framework.ExpectNoError(err)

			ginkgo.By("Validating node labels")
			for _, node := range k8sNodes.Items {
				link := linkMap[node.Name]
				framework.ExpectHaveKeyWithValue(node.Labels, fmt.Sprintf(util.ProviderNetworkInterfaceTemplate, providerNetworkName), link.IfName)
				framework.ExpectHaveKeyWithValue(node.Labels, fmt.Sprintf(util.ProviderNetworkReadyTemplate, providerNetworkName), "true")
				framework.ExpectHaveKeyWithValue(node.Labels, fmt.Sprintf(util.ProviderNetworkMtuTemplate, providerNetworkName), strconv.Itoa(link.Mtu))
				framework.ExpectNotHaveKey(node.Labels, fmt.Sprintf(util.ProviderNetworkExcludeTemplate, providerNetworkName))
			}

			ginkgo.By("Validating provider network spec")
			framework.ExpectEqual(pn.Spec.ExchangeLinkName, false, "field .spec.exchangeLinkName should be false")

			ginkgo.By("Validating provider network status")
			framework.ExpectEqual(pn.Status.Ready, true, "field .status.ready should be true")
			framework.ExpectConsistOf(pn.Status.ReadyNodes, nodeNames)
			framework.ExpectEmpty(pn.Status.Vlans)

			ginkgo.By("Getting kind nodes")
			kindNodes, err := kind.ListNodes(clusterName, "")
			framework.ExpectNoError(err)

			ginkgo.By("Validating node links")
			linkNameMap := make(map[string]string, len(kindNodes))
			bridgeName := util.ExternalBridgeName(providerNetworkName)
			for _, node := range kindNodes {
				if exchangeLinkName {
					bridgeName = linkMap[node.ID].IfName
				}

				links, err := node.ListLinks()
				framework.ExpectNoError(err, "failed to list links on node %s: %v", node.Name(), err)

				var port, bridge *iproute.Link
				for i, link := range links {
					if link.IfIndex == linkMap[node.ID].IfIndex {
						port = &links[i]
					} else if link.IfName == bridgeName {
						bridge = &links[i]
						ginkgo.By("get provider bridge v4 ip " + bridge.AddrInfo[0].Local)
						*bridgeIps = append(*bridgeIps, bridge.AddrInfo[0].Local)
					}
					if port != nil && bridge != nil {
						break
					}
				}
				framework.ExpectNotNil(port)
				framework.ExpectEqual(port.Address, linkMap[node.ID].Address)
				framework.ExpectEqual(port.Mtu, linkMap[node.ID].Mtu)
				framework.ExpectEqual(port.Master, "ovs-system")
				framework.ExpectEqual(port.OperState, "UP")
				if exchangeLinkName {
					framework.ExpectEqual(port.IfName, util.ExternalBridgeName(providerNetworkName))
				}

				framework.ExpectNotNil(bridge)
				framework.ExpectEqual(bridge.LinkInfo.InfoKind, "openvswitch")
				framework.ExpectEqual(bridge.Address, port.Address)
				framework.ExpectEqual(bridge.Mtu, port.Mtu)
				framework.ExpectEqual(bridge.OperState, "UNKNOWN")
				framework.ExpectContainElement(bridge.Flags, "UP")

				framework.ExpectEmpty(port.NonLinkLocalAddresses())
				framework.ExpectConsistOf(bridge.NonLinkLocalAddresses(), linkMap[node.ID].NonLinkLocalAddresses())

				linkNameMap[node.ID] = port.IfName
			}
		}
	})

	ginkgo.AfterEach(func() {
		if containerID == "" {
			os.Exit(1)
			return
		}
		if containerID != "" {
			os.Exit(1)
			return
		}
		if containerID != "" {
			ginkgo.By("Deleting container " + containerID)
			err := docker.ContainerRemove(containerID)
			framework.ExpectNoError(err)
		}

		ginkgo.By("Deleting ovn fip " + fipName)
		ovnFipClient.DeleteSync(fipName)
		ginkgo.By("Deleting ovn dnat " + dnatName)
		ovnDnatRuleClient.DeleteSync(dnatName)
		ginkgo.By("Deleting ovn snat " + snatName)
		ovnSnatRuleClient.DeleteSync(snatName)

		ginkgo.By("Deleting ovn fip " + fipEipName)
		ovnFipClient.DeleteSync(fipEipName)
		ginkgo.By("Deleting ovn eip " + dnatEipName)
		ovnEipClient.DeleteSync(dnatEipName)
		ginkgo.By("Deleting ovn eip " + snatEipName)
		ovnEipClient.DeleteSync(snatEipName)

		ginkgo.By("Deleting ovn allowed address pair vip " + aapVip1Name)
		vipClient.DeleteSync(aapVip1Name)
		ginkgo.By("Deleting ovn allowed address pair vip " + aapVip2Name)
		vipClient.DeleteSync(aapVip2Name)

		// clean up share eip case resource
		ginkgo.By("Deleting share ovn dnat " + sharedEipDnatName)
		ovnDnatRuleClient.DeleteSync(sharedEipDnatName)
		ginkgo.By("Deleting share ovn fip " + sharedEipFipShoudOkName)
		ovnFipClient.DeleteSync(sharedEipFipShoudOkName)
		ginkgo.By("Deleting share ovn fip " + sharedEipFipShoudFailName)
		ovnFipClient.DeleteSync(sharedEipFipShoudFailName)
		ginkgo.By("Deleting share ovn snat " + lrpEipSnatName)
		ovnSnatRuleClient.DeleteSync(lrpEipSnatName)
		ginkgo.By("Deleting share ovn snat " + lrpExtraEipSnatName)
		ovnSnatRuleClient.DeleteSync(lrpExtraEipSnatName)

		// clean up nats with ip or ip cidr
		ginkgo.By("Deleting ovn dnat " + ipDnatName)
		ovnDnatRuleClient.DeleteSync(ipDnatName)
		ginkgo.By("Deleting ovn snat " + ipSnatName)
		ovnSnatRuleClient.DeleteSync(ipSnatName)
		ginkgo.By("Deleting ovn fip " + ipFipName)
		ovnFipClient.DeleteSync(ipFipName)
		ginkgo.By("Deleting ovn snat " + cidrSnatName)
		ovnSnatRuleClient.DeleteSync(cidrSnatName)

		ginkgo.By("Deleting ovn eip " + ipFipEipName)
		ovnFipClient.DeleteSync(ipFipEipName)
		ginkgo.By("Deleting ovn eip " + ipDnatEipName)
		ovnEipClient.DeleteSync(ipDnatEipName)
		ginkgo.By("Deleting ovn eip " + ipSnatEipName)
		ovnEipClient.DeleteSync(ipSnatEipName)
		ginkgo.By("Deleting ovn eip " + cidrSnatEipName)
		ovnEipClient.DeleteSync(cidrSnatEipName)
		ginkgo.By("Deleting ovn eip " + ipFipEipName)
		ovnEipClient.DeleteSync(ipFipEipName)

		ginkgo.By("Deleting ovn vip " + ipFipVipName)
		vipClient.DeleteSync(ipFipVipName)
		ginkgo.By("Deleting ovn vip " + ipDnatVipName)
		vipClient.DeleteSync(ipDnatVipName)
		ginkgo.By("Deleting ovn vip " + ipSnatVipName)
		vipClient.DeleteSync(ipSnatVipName)

		ginkgo.By("Deleting ovn vip " + aapVip2Name)
		vipClient.DeleteSync(aapVip2Name)
		ginkgo.By("Deleting ovn vip " + dnatVipName)
		vipClient.DeleteSync(dnatVipName)
		ginkgo.By("Deleting ovn vip " + fipVipName)
		vipClient.DeleteSync(fipVipName)
		ginkgo.By("Deleting ovn share vip " + sharedVipName)
		vipClient.DeleteSync(sharedVipName)

		// clean fip pod
		ginkgo.By("Deleting pod fip " + podFipName)
		ovnFipClient.DeleteSync(podFipName)
		ginkgo.By("Deleting pod with fip " + fipPodName)
		podClient.DeleteSync(fipPodName)

		// clean fip extra pod
		ginkgo.By("Deleting pod fip " + podExtraFipName)
		ovnFipClient.DeleteSync(podExtraFipName)
		ginkgo.By("Deleting pod with fip " + fipExtraPodName)
		podClient.DeleteSync(fipExtraPodName)
		ginkgo.By("Deleting pod eip " + podExtraEipName)
		ovnEipClient.DeleteSync(podExtraEipName)

		ginkgo.By("Deleting subnet " + noBfdSubnetName)
		subnetClient.DeleteSync(noBfdSubnetName)
		ginkgo.By("Deleting subnet " + noBfdExtraSubnetName)
		subnetClient.DeleteSync(noBfdExtraSubnetName)
		ginkgo.By("Deleting subnet " + bfdSubnetName)
		subnetClient.DeleteSync(bfdSubnetName)

		ginkgo.By("Deleting no bfd custom vpc " + noBfdVpcName)
		vpcClient.DeleteSync(noBfdVpcName)
		ginkgo.By("Deleting bfd custom vpc " + bfdVpcName)
		vpcClient.DeleteSync(bfdVpcName)

		ginkgo.By("Deleting underlay vlan subnet")
		time.Sleep(1 * time.Second)
		// wait 1s to make sure webhook allow delete subnet
		ginkgo.By("Deleting underlay subnet " + underlaySubnetName)
		subnetClient.DeleteSync(underlaySubnetName)
		ginkgo.By("Deleting extra underlay subnet " + underlayExtraSubnetName)
		subnetClient.DeleteSync(underlayExtraSubnetName)

		ginkgo.By("Deleting vlan " + vlanName)
		vlanClient.Delete(vlanName, metav1.DeleteOptions{})
		ginkgo.By("Deleting extra vlan " + vlanExtraName)
		vlanClient.Delete(vlanExtraName, metav1.DeleteOptions{})

		ginkgo.By("Deleting provider network " + providerNetworkName)
		providerNetworkClient.DeleteSync(providerNetworkName)

		ginkgo.By("Deleting provider extra network " + providerExtraNetworkName)
		providerNetworkClient.DeleteSync(providerExtraNetworkName)

		ginkgo.By("Getting nodes")
		nodes, err := kind.ListNodes(clusterName, "")
		framework.ExpectNoError(err, "getting nodes in cluster")

		ginkgo.By("Waiting for ovs bridge to disappear")
		deadline := time.Now().Add(time.Minute)
		for _, node := range nodes {
			err = node.WaitLinkToDisappear(util.ExternalBridgeName(providerNetworkName), 2*time.Second, deadline)
			framework.ExpectNoError(err, "timed out waiting for ovs bridge to disappear in node %s", node.Name())
		}

		if dockerNetwork != nil {
			ginkgo.By("Disconnecting nodes from the docker network")
			err = kind.NetworkDisconnect(dockerNetwork.ID, nodes)
			framework.ExpectNoError(err, "disconnecting nodes from network "+dockerNetworkName)
		}

		if dockerExtraNetwork != nil {
			ginkgo.By("Disconnecting nodes from the docker extra network")
			err = kind.NetworkDisconnect(dockerExtraNetwork.ID, nodes)
			framework.ExpectNoError(err, "disconnecting nodes from extra network "+dockerExtraNetworkName)
		}
	})

	framework.ConformanceIt("Test ovn eip fip snat dnat", func() {
		ginkgo.By("Getting docker network " + dockerNetworkName)
		network, err := docker.NetworkInspect(dockerNetworkName)
		framework.ExpectNoError(err, "getting docker network "+dockerNetworkName)

		exchangeLinkName := false
		itFn(exchangeLinkName, providerNetworkName, linkMap, &providerBridgeIps)

		ginkgo.By("Creating underlay vlan " + vlanName)
		vlan := framework.MakeVlan(vlanName, providerNetworkName, 0)
		_ = vlanClient.Create(vlan)

		ginkgo.By("Creating underlay subnet " + underlaySubnetName)
		cidr := make([]string, 0, 2)
		gateway := make([]string, 0, 2)
		for _, config := range dockerNetwork.IPAM.Config {
			switch util.CheckProtocol(config.Subnet) {
			case kubeovnv1.ProtocolIPv4:
				if f.HasIPv4() {
					cidr = append(cidr, config.Subnet)
					gateway = append(gateway, config.Gateway)
				}
			case kubeovnv1.ProtocolIPv6:
				if f.HasIPv6() {
					cidr = append(cidr, config.Subnet)
					gateway = append(gateway, config.Gateway)
				}
			}
		}
		excludeIPs := make([]string, 0, len(network.Containers)*2)
		for _, container := range network.Containers {
			if container.IPv4Address != "" && f.HasIPv4() {
				excludeIPs = append(excludeIPs, strings.Split(container.IPv4Address, "/")[0])
			}
			if container.IPv6Address != "" && f.HasIPv6() {
				excludeIPs = append(excludeIPs, strings.Split(container.IPv6Address, "/")[0])
			}
		}
		vlanSubnetCidr := strings.Join(cidr, ",")
		vlanSubnetGw := strings.Join(gateway, ",")
		underlaySubnet := framework.MakeSubnet(underlaySubnetName, vlanName, vlanSubnetCidr, vlanSubnetGw, "", "", excludeIPs, nil, nil)
		_ = subnetClient.CreateSync(underlaySubnet)
		vlanSubnet := subnetClient.Get(underlaySubnetName)
		ginkgo.By("Checking underlay vlan " + vlanSubnet.Name)
		framework.ExpectEqual(vlanSubnet.Spec.Vlan, vlanName)
		framework.ExpectNotEqual(vlanSubnet.Spec.CIDRBlock, "")

		externalGwNodes := strings.Join(gwNodeNames, ",")
		ginkgo.By("Creating config map ovn-external-gw-config for centralized case")
		cmData := map[string]string{
			"enable-external-gw": "true",
			"external-gw-nodes":  externalGwNodes,
			"type":               kubeovnv1.GWCentralizedType,
			"external-gw-nic":    "eth1",
			"external-gw-addr":   strings.Join(cidr, ","),
		}
		configMap := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      util.ExternalGatewayConfig,
				Namespace: framework.KubeOvnNamespace,
			},
			Data: cmData,
		}
		_, err = cs.CoreV1().ConfigMaps(framework.KubeOvnNamespace).Create(context.Background(), configMap, metav1.CreateOptions{})
		framework.ExpectNoError(err, "failed to create")

		ginkgo.By("1. Test custom vpc nats using centralized external gw")
		noBfdSubnetV4Cidr := "192.168.0.0/24"
		noBfdSubnetV4Gw := "192.168.0.1"
		enableExternal := true
		disableBfd := false
		noBfdVpc := framework.MakeVpc(noBfdVpcName, "", enableExternal, disableBfd, nil)
		_ = vpcClient.CreateSync(noBfdVpc)
		ginkgo.By("Creating overlay subnet " + noBfdSubnetName)
		noBfdSubnet := framework.MakeSubnet(noBfdSubnetName, "", noBfdSubnetV4Cidr, noBfdSubnetV4Gw, noBfdVpcName, util.OvnProvider, nil, nil, nil)
		_ = subnetClient.CreateSync(noBfdSubnet)
		ginkgo.By("Creating pod on nodes")
		sleepCmd := []string{"sh", "-c", "sleep infinity"}

		for _, node := range nodeNames {
			// create pod on gw node and worker node
			podOnNodeName := fmt.Sprintf("no-bfd-%s", node)
			ginkgo.By("Creating no bfd pod " + podOnNodeName + " with subnet " + noBfdSubnetName)
			annotations := map[string]string{util.LogicalSwitchAnnotation: noBfdSubnetName}
			pod := framework.MakePod(namespaceName, podOnNodeName, nil, annotations, networkToolImage, sleepCmd, nil)
			pod.Spec.NodeName = node
			_ = podClient.CreateSync(pod)
		}

		ginkgo.By("1.1 Test fip dnat snat share eip by setting eip name and ip name")
		ginkgo.By("Create snat with the same vpc lrp eip")
		noBfdlrpEipName := fmt.Sprintf("%s-%s", noBfdVpcName, underlaySubnetName)
		noBfdLrpEip := ovnEipClient.Get(noBfdlrpEipName)
		lrpEipSnat := framework.MakeOvnSnatRule(lrpEipSnatName, noBfdlrpEipName, noBfdSubnetName, "", "", "")
		_ = ovnSnatRuleClient.CreateSync(lrpEipSnat)
		ginkgo.By("Get lrp eip snat")
		lrpEipSnat = ovnSnatRuleClient.Get(lrpEipSnatName)
		ginkgo.By("Check share snat should has the external ip label")
		framework.ExpectHaveKeyWithValue(lrpEipSnat.Labels, util.EipV4IpLabel, noBfdLrpEip.Spec.V4Ip)

		ginkgo.By("Creating pod with eip dnat")
		annotations := map[string]string{util.LogicalSwitchAnnotation: noBfdSubnetName}
		dnatPod := framework.MakePod(namespaceName, fipPodName, nil, annotations, nginxImage, nil, nil)
		dnatPod = podClient.CreateSync(dnatPod)
		dnatEip := framework.MakeOvnEip(dnatEipName, underlaySubnetName, "", "", "", "")
		_ = ovnEipClient.CreateSync(dnatEip)
		dnatPodIpName := ovs.PodNameToPortName(dnatPod.Name, dnatPod.Namespace, noBfdSubnet.Spec.Provider)
		dnatRule := framework.MakeOvnDnatRule(dnatName, dnatEipName, "ip", dnatPodIpName, "", "", "80", "8080", "tcp")

		dnatRule = ovnDnatRuleClient.CreateSync(dnatRule)

		ginkgo.By("Test dnat work for traffic from cluster to dnat eip")

		curlCommandFormat := "curl -s -v --max-time 2 %s"

		nodes, err = kind.ListNodes(clusterName, "")
		framework.ExpectNoError(err, "getting nodes in kind cluster")

		for _, node := range nodes {
			dnatTarget := fmt.Sprintf("%s:8080", dnatRule.Status.V4Eip)

			ginkgo.By("Test node to curl pod dnat eip " + dnatTarget)
			command := fmt.Sprintf(curlCommandFormat, dnatTarget)
			stdOutputBytes, errOutputBytes, err := node.Exec("/bin/sh", "-c", command)

			framework.Logf("output from exec on client node %s curl dst dnat eip %s\n", node.Name())
			if stdOutputBytes != nil && err == nil {
				framework.Logf("output:\n%s", string(stdOutputBytes))
			}
			framework.Logf("exec %s failed err: %v, errOutput: %s, stdOutput: %s", command, err, string(errOutputBytes), string(stdOutputBytes))
		}

		for _, node := range nodeNames {
			// all the pods should ping lrp, node br-external ip successfully
			podOnNodeName := fmt.Sprintf("no-bfd-%s", node)
			pod := podClient.GetPod(podOnNodeName)
			dnatTarget := fmt.Sprintf("%s:8080", dnatRule.Status.V4Eip)

			ginkgo.By("Test pod curl pod dnat eip " + dnatTarget)
			command := fmt.Sprintf(curlCommandFormat, dnatTarget)
			stdOutput, errOutput, err := framework.ExecShellInPod(context.Background(), f, pod.Namespace, pod.Name, command)
			framework.Logf("output from exec on client pod %s curl dst dnat eip %s\n", pod.Name, dnatEip.Name)
			if stdOutput != "" && err == nil {
				framework.Logf("output:\n%s", stdOutput)
			}
			framework.Logf("exec %s failed err: %v, errOutput: %s, stdOutput: %s", command, err, errOutput, stdOutput)
			framework.ExpectNoError(err, "should curl dnat eip success")
		}

	})
})

func init() {
	klog.SetOutput(ginkgo.GinkgoWriter)

	// Register flags.
	config.CopyFlags(config.Flags, flag.CommandLine)
	k8sframework.RegisterCommonFlags(flag.CommandLine)
	k8sframework.RegisterClusterFlags(flag.CommandLine)
}

func TestE2E(t *testing.T) {
	k8sframework.AfterReadingAllFlags(&k8sframework.TestContext)
	e2e.RunE2ETests(t)
}
