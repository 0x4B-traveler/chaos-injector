// Command chaos-injector is the CLI entry point (随机决策层 + 入口).
//
// Subcommands map to fault atoms; "random" exercises the random-decision
// layer with safe parameter ranges and a load gate.
//
// Safety rules
//   - Real injection requires an explicit -confirm flag.
//   - -dry-run validates preconditions and prints the plan, injecting nothing.
//   - Every experiment auto-recovers after -duration seconds.
package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"time"

	"chaos-injector/internal/experiment"
	"chaos-injector/internal/fault"
	"chaos-injector/internal/k8s"
)

const loadLimit = 4.0

type commonFlags struct {
	seconds  float64
	confirm  bool
	dryRun   bool
	timeline string
	seed     int64 // PRNG seed for "random" (-1 = not a random experiment)
}

func (c *commonFlags) duration() time.Duration {
	return time.Duration(c.seconds * float64(time.Second))
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: chaos-injector <network|cpu|mem|process|port|mysql|k8s|random> [flags]")
	fmt.Fprintln(os.Stderr, "       chaos-injector --list")
	fmt.Fprintln(os.Stderr, "       chaos-injector k8s <pod-kill|exec> [flags]")
}

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return 2
	}
	switch args[0] {
	case "--list":
		return listFaults()
	case "network":
		return cmdNetwork(args[1:])
	case "cpu":
		return cmdCPU(args[1:])
	case "mem":
		return cmdMem(args[1:])
	case "process":
		return cmdProcess(args[1:])
	case "port":
		return cmdPort(args[1:])
	case "mysql":
		return cmdMySQL(args[1:])
	case "k8s":
		return cmdK8s(args[1:])
	case "random":
		return cmdRandom(args[1:])
	default:
		usage()
		return 2
	}
}

func addCommonFlags(fs *flag.FlagSet) *commonFlags {
	c := &commonFlags{seed: -1}
	fs.Float64Var(&c.seconds, "duration", 0, "fault lifetime in seconds (auto-rollback after)")
	fs.BoolVar(&c.confirm, "confirm", false, "acknowledge that a real fault will be injected")
	fs.BoolVar(&c.dryRun, "dry-run", false, "validate preconditions and print the plan only")
	fs.StringVar(&c.timeline, "timeline", "", "write the experiment timeline as JSON evidence")
	return c
}

func listFaults() int {
	for _, name := range []string{"network", "cpu", "mem", "process", "port", "pod-kill", "mysql"} {
		f := fault.Registry[name]()
		fmt.Printf("%-10s %s\n", name, f.Description())
	}
	return 0
}

func cmdNetwork(args []string) int {
	fs := flag.NewFlagSet("network", flag.ExitOnError)
	iface := fs.String("iface", "", "network interface to fault (e.g. eth0)")
	delay := fs.Int("delay", 200, "one-way delay in ms")
	loss := fs.Float64("loss", 0, "packet loss percent [0-100]")
	c := addCommonFlags(fs)
	fs.Parse(args)

	f := &fault.NetworkFault{Interface: *iface, DelayMS: *delay, LossPct: *loss}
	return runExperiment("network", f, *c)
}

func cmdCPU(args []string) int {
	fs := flag.NewFlagSet("cpu", flag.ExitOnError)
	load := fs.Int("load", 80, "target load percent [1-100]")
	cores := fs.Int("cores", 1, "number of cores to burn")
	c := addCommonFlags(fs)
	fs.Parse(args)

	f := &fault.CpuFault{LoadPercent: *load, Cores: *cores}
	return runExperiment("cpu", f, *c)
}

func cmdMem(args []string) int {
	fs := flag.NewFlagSet("mem", flag.ExitOnError)
	sizeMB := fs.Int("size-mb", 256, "memory to occupy in MB (must fit in available memory)")
	c := addCommonFlags(fs)
	fs.Parse(args)

	f := &fault.MemFault{SizeMB: *sizeMB}
	return runExperiment("mem", f, *c)
}

func cmdProcess(args []string) int {
	fs := flag.NewFlagSet("process", flag.ExitOnError)
	pattern := fs.String("pattern", "", "kill processes whose name/cmdline contains this pattern")
	c := addCommonFlags(fs)
	fs.Parse(args)

	f := &fault.ProcessFault{Pattern: *pattern}
	return runExperiment("process", f, *c)
}

func cmdPort(args []string) int {
	fs := flag.NewFlagSet("port", flag.ExitOnError)
	port := fs.Int("port", 8080, "TCP port to occupy")
	c := addCommonFlags(fs)
	fs.Parse(args)

	f := &fault.PortFault{Port: *port}
	return runExperiment("port", f, *c)
}

func cmdMySQL(args []string) int {
	fs := flag.NewFlagSet("mysql", flag.ExitOnError)
	host := fs.String("host", "127.0.0.1", "mysql host")
	port := fs.Int("port", 3306, "mysql port")
	user := fs.String("user", "root", "mysql user")
	password := fs.String("password", "", "mysql password (via MYSQL_PWD env, never logged)")
	database := fs.String("database", "", "database to fault against")
	mode := fs.String("mode", "connection", "fault mode: connection|lock|session")
	connections := fs.Int("connections", 20, "connections to occupy (connection mode)")
	table := fs.String("table", "", "table to lock (lock mode)")
	sessionUser := fs.String("session-user", "", "kill sessions of this user (session mode)")
	sessionDB := fs.String("session-db", "", "kill sessions using this database")
	sessionCommand := fs.String("session-command", "", "kill sessions running this command (e.g. Query)")
	sessionPattern := fs.String("session-pattern", "", "kill sessions whose SQL contains this pattern")
	c := addCommonFlags(fs)
	fs.Parse(args)

	f := &fault.MySQLFault{
		Host:           *host,
		Port:           *port,
		User:           *user,
		Password:       *password,
		Database:       *database,
		Mode:           *mode,
		Connections:    *connections,
		Table:          *table,
		SessionUser:    *sessionUser,
		SessionDB:      *sessionDB,
		SessionCommand: *sessionCommand,
		SessionPattern: *sessionPattern,
		Duration:       c.duration(),
	}
	return runExperiment("mysql", f, *c)
}

func k8sUsage() {
	fmt.Fprintln(os.Stderr, "usage: chaos-injector k8s <pod-kill|exec> [flags]")
	fmt.Fprintln(os.Stderr, "  pod-kill  delete a Kubernetes pod (self-healing chaos)")
	fmt.Fprintln(os.Stderr, "  exec      inject an existing fault atom inside a pod via kubectl exec")
}

func cmdK8s(args []string) int {
	if len(args) == 0 {
		k8sUsage()
		return 2
	}
	switch args[0] {
	case "pod-kill":
		return cmdK8sPodKill(args[1:])
	case "exec":
		return cmdK8sExec(args[1:])
	case "help", "-h", "--help":
		k8sUsage()
		return 0
	default:
		k8sUsage()
		return 2
	}
}

func cmdK8sPodKill(args []string) int {
	fs := flag.NewFlagSet("pod-kill", flag.ExitOnError)
	ns := fs.String("namespace", "default", "kubernetes namespace")
	pod := fs.String("pod", "", "pod name to delete (mutually exclusive with -selector)")
	selector := fs.String("selector", "", "label selector of pods to delete")
	count := fs.Int("count", 1, "max number of matching pods to delete")
	waitReady := fs.Int("wait-ready", 60, "seconds to wait for a replacement pod (0 = skip)")
	dryRun := fs.Bool("dry-run", false, "validate preconditions and print the plan only")
	confirm := fs.Bool("confirm", false, "acknowledge that a real fault will be injected")
	timeline := fs.String("timeline", "", "write the experiment timeline as JSON evidence")
	fs.Parse(args)

	f := &fault.PodKillFault{
		Namespace: *ns,
		Pod:       *pod,
		Selector:  *selector,
		Count:     *count,
		WaitReady: time.Duration(*waitReady) * time.Second,
	}

	if *dryRun {
		fmt.Printf("[dry-run] would inject: %s\n", f.Describe())
		return 0
	}
	if !*confirm {
		fmt.Fprintln(os.Stderr, "error: refusing to inject without -confirm")
		return 2
	}

	exp := &experiment.Experiment{
		Name:     "k8s-pod-kill",
		Fault:    f,
		Duration: time.Second, // instantaneous fault: 1s observation window, then no-op Recover
		OnEvent: func(ev experiment.Event) {
			fmt.Printf("  [%s] %-8s %s\n", ev.At.Format("15:04:05.000"), ev.Phase, ev.Detail)
		},
	}
	if err := exp.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	if healed := f.Healed(); healed != "" {
		fmt.Printf("[k8s] replacement ready: %s (self-healing observed)\n", healed)
	}
	if *timeline != "" {
		if err := exp.WriteTimeline(*timeline); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 1
		}
		fmt.Printf("[timeline] written to %s\n", *timeline)
	}
	return 0
}

func cmdK8sExec(args []string) int {
	fs := flag.NewFlagSet("exec", flag.ExitOnError)
	ns := fs.String("namespace", "default", "kubernetes namespace")
	pod := fs.String("pod", "", "pod to inject into")
	agent := fs.String("agent", "", "path to a Linux chaos-injector binary to copy into the pod (default: self on Linux)")
	dryRun := fs.Bool("dry-run", false, "validate preconditions and print the plan only")
	confirm := fs.Bool("confirm", false, "acknowledge that a real fault will be injected")
	fs.Parse(args)

	if *pod == "" {
		fmt.Fprintln(os.Stderr, "error: -pod is required")
		return 2
	}
	rest := fs.Args() // atom + agent flags (Go style: -duration/-confirm/-timeline)
	if len(rest) == 0 {
		fmt.Fprintln(os.Stderr, "error: missing fault atom (network|cpu|mem|process|port|mysql)")
		return 2
	}
	atom := rest[0]
	valid := false
	for _, a := range []string{"network", "cpu", "mem", "process", "port", "mysql"} {
		if atom == a {
			valid = true
			break
		}
	}
	if !valid {
		fmt.Fprintf(os.Stderr, "error: unknown fault atom %q (network|cpu|mem|process|port|mysql)\n", atom)
		return 2
	}
	atomArgs := rest[1:]

	agentPath, err := k8s.AgentPath(*agent)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}

	if *dryRun {
		fmt.Printf("[dry-run] would copy %s to pod %s/%s and run: %s %s %v\n",
			agentPath, *ns, *pod, k8s.AgentRemotePath, atom, atomArgs)
		return 0
	}
	if !*confirm {
		fmt.Fprintln(os.Stderr, "error: refusing to inject without -confirm")
		return 2
	}

	if err := k8s.RunPodExperiment(k8s.New(), *ns, *pod, agentPath, atom, atomArgs); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}

func cmdRandom(args []string) int {
	fs := flag.NewFlagSet("random", flag.ExitOnError)
	iface := fs.String("iface", "", "enable network faults in the random pool (Linux only)")
	seed := fs.Int64("seed", -1, "PRNG seed for reproducible experiments (same seed -> same fault, "+
		"identical to the Python implementation; -1 = random)")
	c := addCommonFlags(fs)
	fs.Parse(args)

	// Environment awareness: refuse to inject into a host already under load.
	ok, detail := fault.LoadGate(loadLimit)
	fmt.Printf("[env] %s\n", detail)
	if !ok {
		fmt.Fprintln(os.Stderr, "error: refusing to inject: host already under load")
		return 2
	}

	// A seed is always recorded (user-provided or fresh) so the experiment
	// can be replayed later.
	if *seed < 0 {
		*seed = time.Now().UnixNano()
	}
	c.seed = *seed
	rng := fault.NewSeededRng(*seed)
	fmt.Printf("[random] seed=%d\n", *seed)

	kinds := []string{"cpu", "mem", "port"}
	if *iface != "" && runtime.GOOS == "linux" {
		kinds = append(kinds, "network")
	}
	var f fault.Fault
	switch kinds[rng.IntN(len(kinds))] {
	case "network":
		f = &fault.NetworkFault{
			Interface: *iface,
			DelayMS:   []int{50, 100, 200, 300, 500}[rng.IntN(5)],
			LossPct:   []float64{0, 1, 3, 5, 10}[rng.IntN(5)],
		}
	case "mem":
		// Deterministic range; OOM protection is enforced by MemFault.Check.
		f = &fault.MemFault{SizeMB: rng.IntRange(64, 256)}
	case "port":
		f = &fault.PortFault{Port: rng.IntRange(8000, 9999)}
	default:
		f = &fault.CpuFault{
			LoadPercent: rng.IntRange(30, 90),
			Cores:       rng.IntRange(1, 4),
		}
	}
	fmt.Printf("[env] randomly chosen %s\n", f.Describe())
	return runExperiment("random", f, *c)
}

func runExperiment(name string, f fault.Fault, c commonFlags) int {
	if c.dryRun {
		fmt.Printf("[dry-run] would inject: %s\n", f.Describe())
		return 0
	}
	if !c.confirm {
		fmt.Fprintln(os.Stderr, "error: refusing to inject without -confirm")
		return 2
	}

	exp := &experiment.Experiment{
		Name:     name,
		Fault:    f,
		Duration: c.duration(),
		Seed:     c.seed,
		SeedSet:  c.seed >= 0,
		OnEvent: func(ev experiment.Event) {
			fmt.Printf("  [%s] %-8s %s\n", ev.At.Format("15:04:05.000"), ev.Phase, ev.Detail)
		},
	}
	if err := exp.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	if c.timeline != "" {
		if err := exp.WriteTimeline(c.timeline); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 1
		}
		fmt.Printf("[timeline] written to %s\n", c.timeline)
	}
	return 0
}
