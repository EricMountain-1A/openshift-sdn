package ovs_subnet

import (
	"errors"
	"fmt"
	log "github.com/golang/glog"
	"net"
	"time"

	"github.com/openshift/openshift-sdn/ovs_subnet/default_controller"
	"github.com/openshift/openshift-sdn/ovs_subnet/kube_controller"
	"github.com/openshift/openshift-sdn/pkg/netutils"
	"github.com/openshift/openshift-sdn/pkg/registry"
)

type OvsController struct {
	subnetRegistry  registry.SubnetRegistry
	localIP         string
	localSubnet     *registry.Subnet
	hostName        string
	subnetAllocator *netutils.SubnetAllocator
	sig             chan struct{}
	flowController  FlowController
}

type FlowController interface {
	Setup(localSubnet, globalSubnet string) error
	AddOFRules(minionIP, localSubnet, localIP string) error
	DelOFRules(minionIP, localIP string) error
}

func NewKubeController(sub registry.SubnetRegistry, hostname string, selfIP string) (*OvsController, error) {
	kubeController, err := NewController(sub, hostname, selfIP)
	if err == nil {
		kubeController.flowController = kube_controller.NewFlowController()
	}
	return kubeController, err
}

func NewDefaultController(sub registry.SubnetRegistry, hostname string, selfIP string) (*OvsController, error) {
	defaultController, err := NewController(sub, hostname, selfIP)
	if err == nil {
		defaultController.flowController = default_controller.NewFlowController()
	}
	return defaultController, err
}

func NewController(sub registry.SubnetRegistry, hostname string, selfIP string) (*OvsController, error) {
	if selfIP == "" {
		addrs, err := net.LookupIP(hostname)
		if err != nil {
			log.Errorf("Failed to lookup IP Address for %s", hostname)
			return nil, err
		}
		selfIP = addrs[0].String()
	}
	log.Infof("Self IP: %s.", selfIP)
	return &OvsController{
		subnetRegistry:  sub,
		localIP:         selfIP,
		hostName:        hostname,
		localSubnet:     nil,
		subnetAllocator: nil,
		sig:             make(chan struct{}),
	}, nil
}

func (oc *OvsController) StartMaster(sync bool, containerNetwork string, containerSubnetLength uint) error {
	// wait a minute for etcd to come alive
	status := oc.subnetRegistry.CheckEtcdIsAlive(60)
	if !status {
		log.Errorf("Etcd not running?")
		return errors.New("Etcd not reachable. Sync cluster check failed.")
	}
	// initialize the minion key
	if sync {
		err := oc.subnetRegistry.InitMinions()
		if err != nil {
			log.Infof("Minion path already initialized.")
		}
	}

	// initialize the subnet key?
	err := oc.subnetRegistry.InitSubnets()
	subrange := make([]string, 0)
	if err != nil {
		subnets, err := oc.subnetRegistry.GetSubnets()
		if err != nil {
			log.Errorf("Error in initializing/fetching subnets: %v", err)
			return err
		}
		for _, sub := range *subnets {
			subrange = append(subrange, sub.Sub)
		}
	}

	err = oc.subnetRegistry.WriteNetworkConfig(containerNetwork, containerSubnetLength)
	if err != nil {
		return err
	}

	oc.subnetAllocator, err = netutils.NewSubnetAllocator(containerNetwork, containerSubnetLength, subrange)
	if err != nil {
		return err
	}
	err = oc.ServeExistingMinions()
	if err != nil {
		log.Warningf("Error initializing existing minions: %v", err)
		// no worry, we can still keep watching it.
	}
	go oc.watchMinions()
	return nil
}

func (oc *OvsController) ServeExistingMinions() error {
	minions, err := oc.subnetRegistry.GetMinions()
	if err != nil {
		return err
	}

	for _, minion := range *minions {
		_, err := oc.subnetRegistry.GetSubnet(minion)
		if err == nil {
			// subnet already exists, continue
			continue
		}
		err = oc.AddNode(minion)
		if err != nil {
			return err
		}
	}
	return nil
}

func (oc *OvsController) AddNode(minion string) error {
	sn, err := oc.subnetAllocator.GetNetwork()
	if err != nil {
		log.Errorf("Error creating network for minion %s.", minion)
		return err
	}
	var minionIP string
	ip := net.ParseIP(minion)
	if ip == nil {
		addrs, err := net.LookupIP(minion)
		if err != nil {
			log.Errorf("Failed to lookup IP address for minion %s: %v", minion, err)
			return err
		}
		minionIP = addrs[0].String()
		if minionIP == "" {
			return fmt.Errorf("Failed to obtain IP address from minion label: %s", minion)
		}
	} else {
		minionIP = ip.String()
	}
	sub := &registry.Subnet{
		Minion: minionIP,
		Sub:    sn.String(),
	}
	oc.subnetRegistry.CreateSubnet(minion, sub)
	if err != nil {
		log.Errorf("Error writing subnet to etcd for minion %s: %v", minion, sn)
		return err
	}
	return nil
}

func (oc *OvsController) DeleteNode(minion string) error {
	sub, err := oc.subnetRegistry.GetSubnet(minion)
	if err != nil {
		log.Errorf("Error fetching subnet for minion %s for delete operation.", minion)
		return err
	}
	_, ipnet, err := net.ParseCIDR(sub.Sub)
	if err != nil {
		log.Errorf("Error parsing subnet for minion %s for deletion: %s", minion, sub.Sub)
		return err
	}
	oc.subnetAllocator.ReleaseNetwork(ipnet)
	return oc.subnetRegistry.DeleteSubnet(minion)
}

func (oc *OvsController) syncWithMaster() error {
	return oc.subnetRegistry.CreateMinion(oc.hostName, oc.localIP)
}

func (oc *OvsController) StartNode(sync, skipsetup bool) error {
	if sync {
		err := oc.syncWithMaster()
		if err != nil {
			log.Errorf("Failed to register with master: %v", err)
			return err
		}
	}
	err := oc.initSelfSubnet()
	if err != nil {
		log.Errorf("Failed to get subnet for this host: %v", err)
		return err
	}
	// call flow controller's setup
	if err == nil {
		if !skipsetup {
			// Assume we are working with IPv4
			containerNetwork, err := oc.subnetRegistry.GetContainerNetwork()
			if err != nil {
				log.Errorf("Failed to obtain ContainerNetwork: %v", err)
				return err
			}
			err = oc.flowController.Setup(oc.localSubnet.Sub, containerNetwork)
			if err != nil {
				return err
			}
		}
		subnets, err := oc.subnetRegistry.GetSubnets()
		if err != nil {
			log.Errorf("Could not fetch existing subnets: %v", err)
		}
		for _, s := range *subnets {
			oc.flowController.AddOFRules(s.Minion, s.Sub, oc.localIP)
		}
		go oc.watchCluster()
	}
	return err
}

func (oc *OvsController) initSelfSubnet() error {
	// get subnet for self
	for {
		sub, err := oc.subnetRegistry.GetSubnet(oc.hostName)
		if err != nil {
			log.Errorf("Could not find an allocated subnet for minion %s: %s. Waiting...", oc.hostName, err)
			time.Sleep(2 * time.Second)
			continue
		}
		oc.localSubnet = sub
		return nil
	}
}

func (oc *OvsController) watchMinions() {
	// watch latest?
	stop := make(chan bool)
	minevent := make(chan *registry.MinionEvent)
	go oc.subnetRegistry.WatchMinions(0, minevent, stop)
	for {
		select {
		case ev := <-minevent:
			switch ev.Type {
			case registry.Added:
				_, err := oc.subnetRegistry.GetSubnet(ev.Minion)
				if err != nil {
					// subnet does not exist already
					oc.AddNode(ev.Minion)
				}
			case registry.Deleted:
				oc.DeleteNode(ev.Minion)
			}
		case <-oc.sig:
			log.Error("Signal received. Stopping watching of minions.")
			stop <- true
			return
		}
	}
}

func (oc *OvsController) watchCluster() {
	stop := make(chan bool)
	clusterEvent := make(chan *registry.SubnetEvent)
	go oc.subnetRegistry.WatchSubnets(0, clusterEvent, stop)
	for {
		select {
		case ev := <-clusterEvent:
			switch ev.Type {
			case registry.Added:
				// add openflow rules
				oc.flowController.AddOFRules(ev.Sub.Minion, ev.Sub.Sub, oc.localIP)
			case registry.Deleted:
				// delete openflow rules meant for the minion
				oc.flowController.DelOFRules(ev.Sub.Minion, oc.localIP)
			}
		case <-oc.sig:
			stop <- true
			return
		}
	}
}

func (oc *OvsController) Stop() {
	close(oc.sig)
	//oc.sig <- struct{}{}
}
