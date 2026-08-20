//go:build unix

package fault

import "os"

func requireRoot() error {
	if os.Geteuid() != 0 {
		return Errf("NetworkFault requires root (tc netem)")
	}
	return nil
}
