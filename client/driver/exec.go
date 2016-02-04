package driver

import (
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/hashicorp/go-plugin"
	"github.com/hashicorp/nomad/client/allocdir"
	"github.com/hashicorp/nomad/client/config"
	"github.com/hashicorp/nomad/client/driver/plugins"
	cstructs "github.com/hashicorp/nomad/client/driver/structs"
	"github.com/hashicorp/nomad/client/getter"
	"github.com/hashicorp/nomad/helper/discover"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/mitchellh/mapstructure"
)

// ExecDriver fork/execs tasks using as many of the underlying OS's isolation
// features.
type ExecDriver struct {
	DriverContext
}

type ExecDriverConfig struct {
	ArtifactSource string   `mapstructure:"artifact_source"`
	Checksum       string   `mapstructure:"checksum"`
	Command        string   `mapstructure:"command"`
	Args           []string `mapstructure:"args"`
}

// execHandle is returned from Start/Open as a handle to the PID
type execHandle struct {
	pluginClient *plugin.Client
	executor     plugins.Executor
	userPid      int
	killTimeout  time.Duration
	logger       *log.Logger
	waitCh       chan *cstructs.WaitResult
	doneCh       chan struct{}
}

// NewExecDriver is used to create a new exec driver
func NewExecDriver(ctx *DriverContext) Driver {
	return &ExecDriver{DriverContext: *ctx}
}

func (d *ExecDriver) Fingerprint(cfg *config.Config, node *structs.Node) (bool, error) {
	// Only enable if cgroups are available and we are root
	if _, ok := node.Attributes["unique.cgroup.mountpoint"]; !ok {
		d.logger.Printf("[DEBUG] driver.exec: cgroups unavailable, disabling")
		return false, nil
	} else if syscall.Geteuid() != 0 {
		d.logger.Printf("[DEBUG] driver.exec: must run as root user, disabling")
		return false, nil
	}

	node.Attributes["driver.exec"] = "1"
	return true, nil
}

func (d *ExecDriver) Periodic() (bool, time.Duration) {
	return true, 15 * time.Second
}

func (d *ExecDriver) Start(ctx *ExecContext, task *structs.Task) (DriverHandle, error) {
	var driverConfig ExecDriverConfig
	if err := mapstructure.WeakDecode(task.Config, &driverConfig); err != nil {
		return nil, err
	}
	// Get the command to be ran
	command := driverConfig.Command
	if command == "" {
		return nil, fmt.Errorf("missing command for exec driver")
	}

	// Create a location to download the artifact.
	taskDir, ok := ctx.AllocDir.TaskDirs[d.DriverContext.taskName]
	if !ok {
		return nil, fmt.Errorf("Could not find task directory for task: %v", d.DriverContext.taskName)
	}

	// Check if an artificat is specified and attempt to download it
	source, ok := task.Config["artifact_source"]
	if ok && source != "" {
		// Proceed to download an artifact to be executed.
		_, err := getter.GetArtifact(
			filepath.Join(taskDir, allocdir.TaskLocal),
			driverConfig.ArtifactSource,
			driverConfig.Checksum,
			d.logger,
		)
		if err != nil {
			return nil, err
		}
	}

	bin, err := discover.NomadExecutable()
	if err != nil {
		return nil, fmt.Errorf("unable to find the nomad binary: %v", err)
	}
	pluginConfig := &plugin.ClientConfig{
		Cmd: exec.Command(bin, "executor"),
	}

	executor, pluginClient, err := createExecutor(pluginConfig, d.config.LogOutput)
	if err != nil {
		return nil, err
	}
	executorCtx := &plugins.ExecutorContext{
		TaskEnv:          d.taskEnv,
		AllocDir:         ctx.AllocDir,
		TaskName:         task.Name,
		TaskResources:    task.Resources,
		ResourceLimits:   true,
		FSIsolation:      true,
		UnprivilegedUser: false,
	}
	ps, err := executor.LaunchCmd(&plugins.ExecCommand{Cmd: command, Args: driverConfig.Args}, executorCtx)
	if err != nil {
		pluginClient.Kill()
		return nil, fmt.Errorf("error starting process via the plugin: %v", err)
	}
	d.logger.Printf("started process via plugin with pid: %v", ps.Pid)

	// Return a driver handle
	h := &execHandle{
		pluginClient: pluginClient,
		userPid:      ps.Pid,
		executor:     executor,
		killTimeout:  d.DriverContext.KillTimeout(task),
		logger:       d.logger,
		doneCh:       make(chan struct{}),
		waitCh:       make(chan *cstructs.WaitResult, 1),
	}
	go h.run()
	return h, nil
}

type execId struct {
	KillTimeout  time.Duration
	UserPid      int
	PluginConfig *plugins.ExecutorReattachConfig
}

func (d *ExecDriver) Open(ctx *ExecContext, handleID string) (DriverHandle, error) {
	id := &execId{}
	if err := json.Unmarshal([]byte(handleID), id); err != nil {
		return nil, fmt.Errorf("Failed to parse handle '%s': %v", handleID, err)
	}

	reattachConfig := id.PluginConfig.PluginConfig()
	pluginConfig := &plugin.ClientConfig{
		Reattach: reattachConfig,
	}
	executor, client, err := createExecutor(pluginConfig, d.config.LogOutput)
	if err != nil {
		d.logger.Println("[ERROR] error connecting to plugin so destroying plugin pid and user pid")
		if e := destroyPlugin(id.PluginConfig.Pid, id.UserPid); e != nil {
			d.logger.Printf("[ERROR] error destrouing plugin and userpid: %v", e)
		}
		return nil, fmt.Errorf("error connecting to plugin: %v", err)
	}

	// Return a driver handle
	h := &execHandle{
		pluginClient: client,
		executor:     executor,
		userPid:      id.UserPid,
		logger:       d.logger,
		killTimeout:  id.KillTimeout,
		doneCh:       make(chan struct{}),
		waitCh:       make(chan *cstructs.WaitResult, 1),
	}
	go h.run()
	return h, nil
}

func (h *execHandle) ID() string {
	id := execId{
		KillTimeout:  h.killTimeout,
		PluginConfig: plugins.NewExecutorReattachConfig(h.pluginClient.ReattachConfig()),
		UserPid:      h.userPid,
	}

	data, err := json.Marshal(id)
	if err != nil {
		h.logger.Printf("[ERR] driver.exec: failed to marshal ID to JSON: %s", err)
	}
	return string(data)
}

func (h *execHandle) WaitCh() chan *cstructs.WaitResult {
	return h.waitCh
}

func (h *execHandle) Update(task *structs.Task) error {
	// Store the updated kill timeout.
	h.killTimeout = task.KillTimeout

	// Update is not possible
	return nil
}

func (h *execHandle) Kill() error {
	h.executor.ShutDown()
	select {
	case <-h.doneCh:
		return nil
	case <-time.After(h.killTimeout):
		if h.pluginClient.Exited() {
			return nil
		}
		err := h.executor.Exit()
		return err
	}
}

func (h *execHandle) run() {
	ps, err := h.executor.Wait()
	close(h.doneCh)
	h.waitCh <- &cstructs.WaitResult{ExitCode: ps.ExitCode, Signal: 0, Err: err}
	close(h.waitCh)
	h.pluginClient.Kill()
}
