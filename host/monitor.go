package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/flynn/flynn/discoverd/client"
)

type ClusterMetadata struct {
	MonitorEnabled bool `json:"monitor_enabled,omitempty"`
}

type Monitor struct {
	dm *DiscoverdManager
}

func NewMonitor(dm *DiscoverdManager) *Monitor {
	return &Monitor{
		dm: dm,
	}
}

func (m *Monitor) Run() error {
	for {
		if m.dm.localConnected() {
			break
		}
		time.Sleep(1 * time.Second)
		fmt.Println("waiting for local discoverd to come up")
	}

	fmt.Println("waiting for raft leader")

	// Will be set by dm by the time localConnected returns true
	host := os.Getenv("DISCOVERD")
	for {
		// maybe we should add the raft methods to the discoverd client
		resp, err := http.Get(fmt.Sprintf("http://%s/raft/leader", host))
		if err != nil {
			time.Sleep(1 * time.Second)
			continue
		}
		resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			fmt.Println("raft leader elected")
		}
	}

	hostSvc := discoverd.NewService("flynn-host")

	for {
		hostMeta, err := hostSvc.GetMeta()
		if err != nil {
			time.Sleep(5 * time.Second)
			continue
		}
		var clusterMeta ClusterMetadata
		if err := json.Unmarshal(hostMeta.Data, &clusterMeta); err != nil {
			return fmt.Errorf("error decoding cluster meta")
		}
		if clusterMeta.MonitorEnabled {
			break
		}
		time.Sleep(5 * time.Second)
	}

	for {
		_, err := discoverd.DefaultClient.AddServiceAndRegisterInstance("cluster-monitor", m.dm.inst)
		if err == nil {
			break
		}
		time.Sleep(5 * time.Second)
	}
	// and try become elected as the leader.

	// flynn-host cluster client should be working because discoverd is up everywhere

	// if the postgres state is not populated then there is nothing we
	// can do.
	// decode the postgres state, work out which hosts we need to wait
	// for and block until they are up.

	// ensure flannel is running on each node, else get it running

	// ensure postgres is in a good state, else fix it

	// ensure controller is running, else get it running
	return nil
}
