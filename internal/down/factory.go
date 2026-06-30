package down

import (
	"fmt"

	"github.com/lesomnus/gantry/cmd/config"
)

// NewTarget builds a Target for one config entry. A new downstream kind is a
// single case here plus its own file.
func NewTarget(c config.TargetConfig) (Target, error) {
	switch c.Kind {
	case "docker":
		return newDockerTarget(c)
	case "containerd":
		return newContainerdTarget(c)
	default:
		return nil, fmt.Errorf("unknown target kind %q", c.Kind)
	}
}
