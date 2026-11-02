// Package nova polls the Compute API: servers, instance actions and quota limits.
package nova

import (
	"encoding/json"

	"github.com/krisiasty/osmex/internal/osclient"
)

// Spec identifies the Compute API. Microversion 2.96 embeds flavors (2.47), paginates instance
// actions (2.58) and returns minimal records for servers in down cells (2.69).
var Spec = osclient.ServiceSpec{Types: []string{"compute"}, Microversion: "compute 2.96"}

type server struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	Status     string          `json:"status"`
	Created    string          `json:"created"`
	Updated    string          `json:"updated"`
	HostID     string          `json:"hostId"`
	Image      json.RawMessage `json:"image"` // "" for boot-from-volume, otherwise an object
	Flavor     flavor          `json:"flavor"`
	AZ         string          `json:"OS-EXT-AZ:availability_zone"`
	PowerState *int            `json:"OS-EXT-STS:power_state"`
	VMState    string          `json:"OS-EXT-STS:vm_state"`
	TaskState  string          `json:"OS-EXT-STS:task_state"`
	LaunchedAt string          `json:"OS-SRV-USG:launched_at"`
	Locked     bool            `json:"locked"`
	Fault      *fault          `json:"fault"`
}

type flavor struct {
	OriginalName string `json:"original_name"`
	VCPUs        int    `json:"vcpus"`
	RAM          int    `json:"ram"`  // MiB
	Disk         int    `json:"disk"` // GiB
}

type fault struct {
	Code    int    `json:"code"`
	Created string `json:"created"`
}

// downCell reports whether s is a minimal record for a server in an unreachable cell.
func (s server) downCell() bool { return s.Status == "UNKNOWN" }

func imageID(raw json.RawMessage) string {
	var img struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &img) != nil {
		return ""
	}
	return img.ID
}
