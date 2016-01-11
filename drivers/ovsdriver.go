/***
Copyright 2014 Cisco Systems Inc. All rights reserved.

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

package drivers

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	log "github.com/Sirupsen/logrus"
	"github.com/vishvananda/netlink"

	"github.com/contiv/netplugin/core"
	"github.com/contiv/netplugin/netmaster/mastercfg"
)

type oper int

const maxIntfRetry = 100

// OvsDriverConfig defines the configuration required to initialize the
// OvsDriver.
type OvsDriverConfig struct {
	Ovs struct {
		DbIP   string
		DbPort int
	}
}

// OvsDriverOperState carries operational state of the OvsDriver.
type OvsDriverOperState struct {
	core.CommonState
	// used to allocate port names. XXX: should it be user controlled?
	CurrPortNum int `json:"currPortNum"`
}

// Write the state
func (s *OvsDriverOperState) Write() error {
	key := fmt.Sprintf(ovsOperPath, s.ID)
	return s.StateDriver.WriteState(key, s, json.Marshal)
}

// Read the state given an ID.
func (s *OvsDriverOperState) Read(id string) error {
	key := fmt.Sprintf(ovsOperPath, id)
	return s.StateDriver.ReadState(key, s, json.Unmarshal)
}

// ReadAll reads all the state
func (s *OvsDriverOperState) ReadAll() ([]core.State, error) {
	return s.StateDriver.ReadAllState(ovsOperPathPrefix, s, json.Unmarshal)
}

// Clear removes the state.
func (s *OvsDriverOperState) Clear() error {
	key := fmt.Sprintf(ovsOperPath, s.ID)
	return s.StateDriver.ClearState(key)
}

// OvsDriver implements the Layer 2 Network and Endpoint Driver interfaces
// specific to vlan based open-vswitch.
type OvsDriver struct {
	oper     OvsDriverOperState    // Oper state of the driver
	localIP  string                // Local IP address
	switchDb map[string]*OvsSwitch // OVS switch instances
}

func (d *OvsDriver) getIntfName() (string, error) {
	// get the next available port number
	for i := 0; i < maxIntfRetry; i++ {
		// Pick next port number
		d.oper.CurrPortNum++
		intfName := fmt.Sprintf("vport%d", d.oper.CurrPortNum)

		// check if the port name is already in use
		_, err := netlink.LinkByName(intfName)
		if err != nil && strings.Contains(err.Error(), "not found") {
			// save the new state
			err = d.oper.Write()
			if err != nil {
				return "", err
			}
			return intfName, nil
		}
	}

	return "", core.Errorf("Could not get intf name. Max retry exceeded")
}

// Init initializes the OVS driver.
func (d *OvsDriver) Init(config *core.Config, info *core.InstanceInfo) error {

	if config == nil || info == nil || info.StateDriver == nil {
		return core.Errorf("Invalid arguments. cfg: %+v, instance-info: %+v",
			config, info)
	}

	_, ok := config.V.(*OvsDriverConfig)
	if !ok {
		return core.Errorf("Invalid type passed")
	}

	d.oper.StateDriver = info.StateDriver
	d.localIP = info.VtepIP
	// restore the driver's runtime state if it exists
	err := d.oper.Read(info.HostLabel)
	if core.ErrIfKeyExists(err) != nil {
		log.Printf("Failed to read driver oper state for key %q. Error: %s",
			info.HostLabel, err)
		return err
	} else if err != nil {
		// create the oper state as it is first time start up
		d.oper.ID = info.HostLabel
		d.oper.CurrPortNum = 0
		err = d.oper.Write()
		if err != nil {
			return err
		}
	}

	log.Infof("Initializing ovsdriver")

	// Init switch DB
	d.switchDb = make(map[string]*OvsSwitch)

	// Create Vxlan switch
	d.switchDb["vxlan"], err = NewOvsSwitch(vxlanBridgeName, "vxlan", info.VtepIP)
	if err != nil {
		log.Fatalf("Error creating vlan switch. Err: %v", err)
	}
	log.Infof("NEW OVS SWITCH", info.VtepIP)
	// Create Vlan switch
	d.switchDb["vlan"], err = NewOvsSwitch(vlanBridgeName, "vlan", info.VtepIP, info.RouterIP, info.VlanIntf)
	if err != nil {
		log.Fatalf("Error creating vlan switch. Err: %v", err)
	}

	// Add uplink to VLAN switch
	if info.VlanIntf != "" {
		err = d.switchDb["vlan"].AddUplinkPort(info.VlanIntf)
		if err != nil {
			log.Errorf("Could not add uplink %s to vlan OVS. Err: %v", info.VlanIntf, err)
		}
	}

	return nil
}

// Deinit performs cleanup prior to destruction of the OvsDriver
func (d *OvsDriver) Deinit() {
	log.Infof("Cleaning up ovsdriver")

	// cleanup both vlan and vxlan OVS instances
	if d.switchDb["vlan"] != nil {
		d.switchDb["vlan"].Delete()
	}
	if d.switchDb["vxlan"] != nil {
		d.switchDb["vxlan"].Delete()
	}
}

// CreateNetwork creates a network by named identifier
func (d *OvsDriver) CreateNetwork(id string) error {
	cfgNw := mastercfg.CfgNetworkState{}
	cfgNw.StateDriver = d.oper.StateDriver
	err := cfgNw.Read(id)
	if err != nil {
		log.Errorf("Failed to read net %s \n", cfgNw.ID)
		return err
	}
	log.Infof("create net %+v \n", cfgNw)

	// Find the switch based on network type
	var sw *OvsSwitch
	if cfgNw.PktTagType == "vxlan" {
		sw = d.switchDb["vxlan"]
	} else {
		sw = d.switchDb["vlan"]
	}

	return sw.CreateNetwork(uint16(cfgNw.PktTag), uint32(cfgNw.ExtPktTag), cfgNw.Gateway)
}

// DeleteNetwork deletes a network by named identifier
func (d *OvsDriver) DeleteNetwork(id, encap string, pktTag, extPktTag int) error {
	log.Infof("delete net %s, encap %s, tags: %d/%d", id, encap, pktTag, extPktTag)

	// Find the switch based on network type
	var sw *OvsSwitch
	if encap == "vxlan" {
		sw = d.switchDb["vxlan"]
	} else {
		sw = d.switchDb["vlan"]
	}

	return sw.DeleteNetwork(uint16(cfgNw.PktTag), uint32(cfgNw.ExtPktTag), cfgNw.Gateway)
}

// CreateEndpoint creates an endpoint by named identifier
func (d *OvsDriver) CreateEndpoint(id string) error {
	var (
		err      error
		intfName string
	)

	cfgEp := &mastercfg.CfgEndpointState{}
	cfgEp.StateDriver = d.oper.StateDriver
	err = cfgEp.Read(id)
	if err != nil {
		return err
	}

	cfgEpGroup := &mastercfg.EndpointGroupState{}
	cfgEpGroup.StateDriver = d.oper.StateDriver
	err = cfgEpGroup.Read(strconv.Itoa(cfgEp.EndpointGroupID))
	if err == nil {
		log.Debugf("pktTag: %v ", cfgEpGroup.PktTag)
	} else if core.ErrIfKeyExists(err) == nil {
		// FIXME: this should be deprecated once we remove old style intent
		// In case EpGroup is not specified, get the tag from nw.
		// this is mainly for the intent based system tests
		log.Warnf("%v will use network based tag ", err)
		cfgNw := mastercfg.CfgNetworkState{}
		cfgNw.StateDriver = d.oper.StateDriver
		err1 := cfgNw.Read(cfgEp.NetID)
		if err1 != nil {
			log.Errorf("Unable to get tag neither epg nor nw")
			return err1
		}

		cfgEpGroup.PktTagType = cfgNw.PktTagType
		cfgEpGroup.PktTag = cfgNw.PktTag
	} else {
		return err
	}

	// Find the switch based on network type
	var sw *OvsSwitch
	if cfgEpGroup.PktTagType == "vxlan" {
		sw = d.switchDb["vxlan"]
	} else {
		sw = d.switchDb["vlan"]
	}

	operEp := &OvsOperEndpointState{}
	operEp.StateDriver = d.oper.StateDriver
	err = operEp.Read(id)
	if core.ErrIfKeyExists(err) != nil {
		return err
	} else if err == nil {
		// check if oper state matches cfg state. In case of mismatch cleanup
		// up the EP and continue add new one. In case of match just return.
		if operEp.Matches(cfgEp) {
			log.Printf("Found matching oper state for ep %s, noop", id)

			// Ask the switch to update the port
			err = sw.UpdatePort(operEp.PortName, cfgEp, cfgEpGroup.PktTag)
			if err != nil {
				log.Errorf("Error creating port %s. Err: %v", intfName, err)
				return err
			}

			return nil
		}
		log.Printf("Found mismatching oper state for Ep, cleaning it. Config: %+v, Oper: %+v",
			cfgEp, operEp)
		d.DeleteEndpoint(operEp.ID)
	}

	// Get the interface name to use
	intfName, err = d.getIntfName()
	if err != nil {
		return err
	}

	// Ask the switch to create the port
	err = sw.CreatePort(intfName, cfgEp, cfgEpGroup.PktTag)
	if err != nil {
		log.Errorf("Error creating port %s. Err: %v", intfName, err)
		return err
	}

	// Save the oper state
	operEp = &OvsOperEndpointState{
		NetID:       cfgEp.NetID,
		AttachUUID:  cfgEp.AttachUUID,
		ContName:    cfgEp.ContName,
		ServiceName: cfgEp.ServiceName,
		IPAddress:   cfgEp.IPAddress,
		MacAddress:  cfgEp.MacAddress,
		IntfName:    cfgEp.IntfName,
		PortName:    intfName,
		HomingHost:  cfgEp.HomingHost,
		VtepIP:      cfgEp.VtepIP}
	operEp.StateDriver = d.oper.StateDriver
	operEp.ID = id
	err = operEp.Write()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			operEp.Clear()
		}
	}()

	return nil
}

// DeleteEndpoint deletes an endpoint by named identifier.
func (d *OvsDriver) DeleteEndpoint(id string) (err error) {

	epOper := OvsOperEndpointState{}
	epOper.StateDriver = d.oper.StateDriver
	err = epOper.Read(id)
	if err != nil {
		return err
	}
	defer func() {
		epOper.Clear()
	}()

	// Get the network state
	cfgNw := mastercfg.CfgNetworkState{}
	cfgNw.StateDriver = d.oper.StateDriver
	err = cfgNw.Read(epOper.NetID)
	if err != nil {
		return err
	}

	// Find the switch based on network type
	var sw *OvsSwitch
	if cfgNw.PktTagType == "vxlan" {
		sw = d.switchDb["vxlan"]
	} else {
		sw = d.switchDb["vlan"]
	}

	err = sw.DeletePort(&epOper)
	if err != nil {
		log.Errorf("Error deleting endpoint: %+v. Err: %v", epOper, err)
	}

	return nil
}

// AddPeerHost adds VTEPs if necessary
func (d *OvsDriver) AddPeerHost(node core.ServiceInfo) error {
	// Nothing to do if this is our own IP
	if node.HostAddr == d.localIP {
		return nil
	}

	log.Infof("CreatePeerHost for %+v", node)

	// Add the VTEP for the peer in vxlan switch.
	err := d.switchDb["vlan"].CreateVtep(node.HostAddr)
	if err != nil {
		log.Errorf("Error adding the VTEP %s. Err: %s", node.HostAddr, err)
		return err
	}

	// Add the VTEP for the peer in vxlan switch.
	err = d.switchDb["vxlan"].CreateVtep(node.HostAddr)
	if err != nil {
		log.Errorf("Error adding the VTEP %s. Err: %s", node.HostAddr, err)
		return err
	}

	return nil
}

// DeletePeerHost deletes associated VTEP
func (d *OvsDriver) DeletePeerHost(node core.ServiceInfo) error {
	// Nothing to do if this is our own IP
	if node.HostAddr == d.localIP {
		return nil
	}

	log.Infof("DeletePeerHost for %+v", node)

	// Remove VTEP from vxlan switch
	err := d.switchDb["vxlan"].DeleteVtep(node.HostAddr)
	// Add the VTEP for the peer in vxlan switch.
	err := d.switchDb["vlan"].DeleteVtep(node.HostAddr)
	if err != nil {
		log.Errorf("Error deleting the VTEP %s. Err: %s", node.HostAddr, err)
		return err
	}

	// Add the VTEP for the peer in vxlan switch.
	err = d.switchDb["vxlan"].DeleteVtep(node.HostAddr)
	if err != nil {
		log.Errorf("Error deleting the VTEP %s. Err: %s", node.HostAddr, err)
		return err
	}

	return nil
}

// AddMaster adds master node
func (d *OvsDriver) AddMaster(node core.ServiceInfo) error {
	log.Infof("AddMaster for %+v", node)

	// Add master to vlan and vxlan datapaths
	err := d.switchDb["vlan"].AddMaster(node)
	if err != nil {
		return err
	}
	err = d.switchDb["vxlan"].AddMaster(node)
	if err != nil {
		return err
	}
	return nil
}

// DeleteMaster deletes master node
func (d *OvsDriver) DeleteMaster(node core.ServiceInfo) error {
	log.Infof("DeleteMaster for %+v", node)

	// Delete master from vlan and vxlan datapaths
	err := d.switchDb["vlan"].DeleteMaster(node)
	if err != nil {
		return err
	}
	err = d.switchDb["vxlan"].DeleteMaster(node)
	if err != nil {
		return err
	}
	return nil
}

// AddBgpNeighbors adds bgp neighbor by named identifier
func (d *OvsDriver) AddBgpNeighbors(id string) error {
	cfg := mastercfg.CfgBgpState{}
	cfg.StateDriver = d.oper.StateDriver
	log.Info("Reading from etcd State %s", id)
	err := cfg.Read(id)
	if err != nil {
		log.Errorf("Failed to read router state %s \n", cfg.Name)
		return err
	}
	log.Infof("create Bgp Server %s \n", cfg.Name)

	// Find the switch based on network type
	var sw *OvsSwitch
	//if cfg.CfgType == "bgp-vxlan" {
	//	sw = d.switchDb["vxlan"]
	//} else {
	sw = d.switchDb["vlan"]
	//}

	return sw.AddBgpNeighbors(cfg.Name, cfg.As, cfg.Neighbor)
}

// DeleteBgpNeighbors deletes a bgp neighbor by named identifier
func (d *OvsDriver) DeleteBgpNeighbors(id string) error {
	log.Infof("delete router state %s \n", id)
	//FixME: We are not maintaining oper state for Bgp
	//Need to Revisit again
	/*
		cfg := mastercfg.CfgBgpState{}
		cfg.StateDriver = d.oper.StateDriver
		err := cfg.Read(id)
		if err != nil {
			log.Errorf("Failed to read router state %s \n", cfg.Name)
			return err
		}
	*/
	// Find the switch based on network type
	var sw *OvsSwitch
	//	if cfg.CfgType == "bgp-vxlan" {
	//		sw = d.switchDb["vxlan"]
	//	} else {
	sw = d.switchDb["vlan"]
	//	}
	return sw.DeleteBgpNeighbors()

}
