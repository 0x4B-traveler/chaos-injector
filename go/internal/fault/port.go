package fault

import (
	"fmt"
	"net"
)

func init() {
	Register("port", func() Fault { return &PortFault{} })
}

// PortFault occupies a TCP port (chaosblade "network port occupy"
// equivalent): the listener grabs the port so a real service on it becomes
// unreachable. Recovery closes the listener.
type PortFault struct {
	Port int

	ln net.Listener
}

func (f *PortFault) Name() string        { return "port" }
func (f *PortFault) Description() string { return "TCP port occupation" }

func (f *PortFault) Describe() string {
	return fmt.Sprintf("port(port=%d)", f.Port)
}

func (f *PortFault) Check() error {
	if f.Port < 1 || f.Port > 65535 {
		return Errf("port must be in [1, 65535]")
	}
	return nil
}

func (f *PortFault) Inject() error {
	if err := f.Check(); err != nil {
		return err
	}
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", f.Port))
	if err != nil {
		return Errf("port %d already in use: %v", f.Port, err)
	}
	f.ln = ln
	return nil
}

func (f *PortFault) Recover() error {
	if f.ln != nil {
		if err := f.ln.Close(); err != nil {
			return Errf("recover port fault failed: %v", err)
		}
		f.ln = nil
	}
	return nil
}
