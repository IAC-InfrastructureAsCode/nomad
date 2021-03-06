package rawexec

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/hashicorp/hcl2/hcl"
	ctestutil "github.com/hashicorp/nomad/client/testutil"
	"github.com/hashicorp/nomad/helper/testlog"
	"github.com/hashicorp/nomad/helper/testtask"
	"github.com/hashicorp/nomad/helper/uuid"
	basePlug "github.com/hashicorp/nomad/plugins/base"
	"github.com/hashicorp/nomad/plugins/drivers"
	dtestutil "github.com/hashicorp/nomad/plugins/drivers/testutils"
	"github.com/hashicorp/nomad/plugins/shared/hclspec"
	"github.com/hashicorp/nomad/plugins/shared/hclutils"
	pstructs "github.com/hashicorp/nomad/plugins/shared/structs"
	"github.com/hashicorp/nomad/testutil"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestMain(m *testing.M) {
	if !testtask.Run() {
		os.Exit(m.Run())
	}
}

func TestRawExecDriver_SetConfig(t *testing.T) {
	t.Parallel()
	require := require.New(t)

	d := NewRawExecDriver(testlog.HCLogger(t))
	harness := dtestutil.NewDriverHarness(t, d)
	defer harness.Kill()

	bconfig := &basePlug.Config{}

	// Disable raw exec.
	config := &Config{}

	var data []byte
	require.NoError(basePlug.MsgPackEncode(&data, config))
	bconfig.PluginConfig = data
	require.NoError(harness.SetConfig(bconfig))
	require.Exactly(config, d.(*Driver).config)

	config.Enabled = true
	config.NoCgroups = true
	data = []byte{}
	require.NoError(basePlug.MsgPackEncode(&data, config))
	bconfig.PluginConfig = data
	require.NoError(harness.SetConfig(bconfig))
	require.Exactly(config, d.(*Driver).config)

	config.NoCgroups = false
	data = []byte{}
	require.NoError(basePlug.MsgPackEncode(&data, config))
	bconfig.PluginConfig = data
	require.NoError(harness.SetConfig(bconfig))
	require.Exactly(config, d.(*Driver).config)
}

func TestRawExecDriver_Fingerprint(t *testing.T) {
	t.Parallel()

	fingerprintTest := func(config *Config, expected *drivers.Fingerprint) func(t *testing.T) {
		return func(t *testing.T) {
			require := require.New(t)
			d := NewRawExecDriver(testlog.HCLogger(t)).(*Driver)
			harness := dtestutil.NewDriverHarness(t, d)
			defer harness.Kill()

			var data []byte
			require.NoError(basePlug.MsgPackEncode(&data, config))
			bconfig := &basePlug.Config{
				PluginConfig: data,
			}
			require.NoError(harness.SetConfig(bconfig))

			fingerCh, err := harness.Fingerprint(context.Background())
			require.NoError(err)
			select {
			case result := <-fingerCh:
				require.Equal(expected, result)
			case <-time.After(time.Duration(testutil.TestMultiplier()) * time.Second):
				require.Fail("timeout receiving fingerprint")
			}
		}
	}

	cases := []struct {
		Name     string
		Conf     Config
		Expected drivers.Fingerprint
	}{
		{
			Name: "Disabled",
			Conf: Config{
				Enabled: false,
			},
			Expected: drivers.Fingerprint{
				Attributes:        nil,
				Health:            drivers.HealthStateUndetected,
				HealthDescription: "disabled",
			},
		},
		{
			Name: "Enabled",
			Conf: Config{
				Enabled: true,
			},
			Expected: drivers.Fingerprint{
				Attributes:        map[string]*pstructs.Attribute{"driver.raw_exec": pstructs.NewBoolAttribute(true)},
				Health:            drivers.HealthStateHealthy,
				HealthDescription: drivers.DriverHealthy,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.Name, fingerprintTest(&tc.Conf, &tc.Expected))
	}
}

func TestRawExecDriver_StartWait(t *testing.T) {
	t.Parallel()
	require := require.New(t)

	d := NewRawExecDriver(testlog.HCLogger(t))
	harness := dtestutil.NewDriverHarness(t, d)
	defer harness.Kill()
	task := &drivers.TaskConfig{
		ID:   uuid.Generate(),
		Name: "test",
	}

	taskConfig := map[string]interface{}{}
	taskConfig["command"] = "go"
	taskConfig["args"] = []string{"version"}

	encodeDriverHelper(require, task, taskConfig)
	cleanup := harness.MkAllocDir(task, false)
	defer cleanup()

	handle, _, err := harness.StartTask(task)
	require.NoError(err)

	ch, err := harness.WaitTask(context.Background(), handle.Config.ID)
	require.NoError(err)
	result := <-ch
	require.Zero(result.ExitCode)
	require.Zero(result.Signal)
	require.False(result.OOMKilled)
	require.NoError(result.Err)
	require.NoError(harness.DestroyTask(task.ID, true))
}

func TestRawExecDriver_StartWaitStop(t *testing.T) {
	t.Parallel()
	require := require.New(t)

	d := NewRawExecDriver(testlog.HCLogger(t))
	harness := dtestutil.NewDriverHarness(t, d)
	defer harness.Kill()

	// Disable cgroups so test works without root
	config := &Config{NoCgroups: true}
	var data []byte
	require.NoError(basePlug.MsgPackEncode(&data, config))
	bconfig := &basePlug.Config{PluginConfig: data}
	require.NoError(harness.SetConfig(bconfig))

	task := &drivers.TaskConfig{
		ID:   uuid.Generate(),
		Name: "test",
	}

	taskConfig := map[string]interface{}{}
	taskConfig["command"] = testtask.Path()
	taskConfig["args"] = []string{"sleep", "100s"}

	encodeDriverHelper(require, task, taskConfig)

	cleanup := harness.MkAllocDir(task, false)
	defer cleanup()

	handle, _, err := harness.StartTask(task)
	require.NoError(err)

	ch, err := harness.WaitTask(context.Background(), handle.Config.ID)
	require.NoError(err)

	require.NoError(harness.WaitUntilStarted(task.ID, 1*time.Second))

	go func() {
		harness.StopTask(task.ID, 2*time.Second, "SIGINT")
	}()

	select {
	case result := <-ch:
		require.Equal(int(unix.SIGINT), result.Signal)
	case <-time.After(10 * time.Second):
		require.Fail("timeout waiting for task to shutdown")
	}

	// Ensure that the task is marked as dead, but account
	// for WaitTask() closing channel before internal state is updated
	testutil.WaitForResult(func() (bool, error) {
		status, err := harness.InspectTask(task.ID)
		if err != nil {
			return false, fmt.Errorf("inspecting task failed: %v", err)
		}
		if status.State != drivers.TaskStateExited {
			return false, fmt.Errorf("task hasn't exited yet; status: %v", status.State)
		}

		return true, nil
	}, func(err error) {
		require.NoError(err)
	})

	require.NoError(harness.DestroyTask(task.ID, true))
}

func TestRawExecDriver_StartWaitRecoverWaitStop(t *testing.T) {
	t.Parallel()
	require := require.New(t)

	d := NewRawExecDriver(testlog.HCLogger(t))
	harness := dtestutil.NewDriverHarness(t, d)
	defer harness.Kill()

	// Disable cgroups so test works without root
	config := &Config{NoCgroups: true}
	var data []byte
	require.NoError(basePlug.MsgPackEncode(&data, config))
	bconfig := &basePlug.Config{PluginConfig: data}
	require.NoError(harness.SetConfig(bconfig))

	task := &drivers.TaskConfig{
		ID:   uuid.Generate(),
		Name: "sleep",
	}
	taskConfig := map[string]interface{}{}
	taskConfig["command"] = testtask.Path()
	taskConfig["args"] = []string{"sleep", "100s"}

	encodeDriverHelper(require, task, taskConfig)

	testtask.SetTaskConfigEnv(task)
	cleanup := harness.MkAllocDir(task, false)
	defer cleanup()

	handle, _, err := harness.StartTask(task)
	require.NoError(err)

	ch, err := harness.WaitTask(context.Background(), task.ID)
	require.NoError(err)

	var waitDone bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		result := <-ch
		require.Error(result.Err)
		waitDone = true
	}()

	originalStatus, err := d.InspectTask(task.ID)
	require.NoError(err)

	d.(*Driver).tasks.Delete(task.ID)

	wg.Wait()
	require.True(waitDone)
	_, err = d.InspectTask(task.ID)
	require.Equal(drivers.ErrTaskNotFound, err)

	err = d.RecoverTask(handle)
	require.NoError(err)

	status, err := d.InspectTask(task.ID)
	require.NoError(err)
	require.Exactly(originalStatus, status)

	ch, err = harness.WaitTask(context.Background(), task.ID)
	require.NoError(err)

	wg.Add(1)
	waitDone = false
	go func() {
		defer wg.Done()
		result := <-ch
		require.NoError(result.Err)
		require.NotZero(result.ExitCode)
		require.Equal(9, result.Signal)
		waitDone = true
	}()

	time.Sleep(300 * time.Millisecond)
	require.NoError(d.StopTask(task.ID, 0, "SIGKILL"))
	wg.Wait()
	require.NoError(d.DestroyTask(task.ID, false))
	require.True(waitDone)

}

func TestRawExecDriver_Start_Wait_AllocDir(t *testing.T) {
	t.Parallel()
	require := require.New(t)

	d := NewRawExecDriver(testlog.HCLogger(t))
	harness := dtestutil.NewDriverHarness(t, d)
	defer harness.Kill()

	task := &drivers.TaskConfig{
		ID:   uuid.Generate(),
		Name: "sleep",
	}

	cleanup := harness.MkAllocDir(task, false)
	defer cleanup()

	exp := []byte("win")
	file := "output.txt"
	outPath := fmt.Sprintf(`%s/%s`, task.TaskDir().SharedAllocDir, file)

	taskConfig := map[string]interface{}{}
	taskConfig["command"] = testtask.Path()
	taskConfig["args"] = []string{"sleep", "1s", "write", string(exp), outPath}
	encodeDriverHelper(require, task, taskConfig)
	testtask.SetTaskConfigEnv(task)

	_, _, err := harness.StartTask(task)
	require.NoError(err)

	// Task should terminate quickly
	waitCh, err := harness.WaitTask(context.Background(), task.ID)
	require.NoError(err)

	select {
	case res := <-waitCh:
		require.NoError(res.Err)
		require.True(res.Successful())
	case <-time.After(time.Duration(testutil.TestMultiplier()*5) * time.Second):
		require.Fail("WaitTask timeout")
	}

	// Check that data was written to the shared alloc directory.
	outputFile := filepath.Join(task.TaskDir().SharedAllocDir, file)
	act, err := ioutil.ReadFile(outputFile)
	require.NoError(err)
	require.Exactly(exp, act)
	require.NoError(harness.DestroyTask(task.ID, true))
}

// This test creates a process tree such that without cgroups tracking the
// processes cleanup of the children would not be possible. Thus the test
// asserts that the processes get killed properly when using cgroups.
func TestRawExecDriver_Start_Kill_Wait_Cgroup(t *testing.T) {
	ctestutil.ExecCompatible(t)
	t.Parallel()
	require := require.New(t)
	pidFile := "pid"

	d := NewRawExecDriver(testlog.HCLogger(t))
	harness := dtestutil.NewDriverHarness(t, d)
	defer harness.Kill()

	task := &drivers.TaskConfig{
		ID:   uuid.Generate(),
		Name: "sleep",
		User: "root",
	}

	cleanup := harness.MkAllocDir(task, false)
	defer cleanup()

	taskConfig := map[string]interface{}{}
	taskConfig["command"] = testtask.Path()
	taskConfig["args"] = []string{"fork/exec", pidFile, "pgrp", "0", "sleep", "20s"}
	encodeDriverHelper(require, task, taskConfig)
	testtask.SetTaskConfigEnv(task)

	_, _, err := harness.StartTask(task)
	require.NoError(err)

	// Find the process
	var pidData []byte
	testutil.WaitForResult(func() (bool, error) {
		var err error
		pidData, err = ioutil.ReadFile(filepath.Join(task.TaskDir().Dir, pidFile))
		if err != nil {
			return false, err
		}

		if len(pidData) == 0 {
			return false, fmt.Errorf("pidFile empty")
		}

		return true, nil
	}, func(err error) {
		require.NoError(err)
	})

	pid, err := strconv.Atoi(string(pidData))
	require.NoError(err, "failed to read pidData: %s", string(pidData))

	// Check the pid is up
	process, err := os.FindProcess(pid)
	require.NoError(err)
	require.NoError(process.Signal(syscall.Signal(0)))

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(1 * time.Second)
		err := harness.StopTask(task.ID, 0, "")

		// Can't rely on the ordering between wait and kill on travis...
		if !testutil.IsTravis() {
			require.NoError(err)
		}
	}()

	// Task should terminate quickly
	waitCh, err := harness.WaitTask(context.Background(), task.ID)
	require.NoError(err)
	select {
	case res := <-waitCh:
		require.False(res.Successful())
	case <-time.After(time.Duration(testutil.TestMultiplier()*5) * time.Second):
		require.Fail("WaitTask timeout")
	}

	testutil.WaitForResult(func() (bool, error) {
		if err := process.Signal(syscall.Signal(0)); err == nil {
			return false, fmt.Errorf("process should not exist: %v", pid)
		}

		return true, nil
	}, func(err error) {
		require.NoError(err)
	})

	wg.Wait()
	require.NoError(harness.DestroyTask(task.ID, true))
}

func TestRawExecDriver_Exec(t *testing.T) {
	t.Parallel()
	require := require.New(t)

	d := NewRawExecDriver(testlog.HCLogger(t))
	harness := dtestutil.NewDriverHarness(t, d)
	defer harness.Kill()

	task := &drivers.TaskConfig{
		ID:   uuid.Generate(),
		Name: "sleep",
	}

	cleanup := harness.MkAllocDir(task, false)
	defer cleanup()

	taskConfig := map[string]interface{}{}
	taskConfig["command"] = testtask.Path()
	taskConfig["args"] = []string{"sleep", "9000s"}
	encodeDriverHelper(require, task, taskConfig)
	testtask.SetTaskConfigEnv(task)

	_, _, err := harness.StartTask(task)
	require.NoError(err)

	// Exec a command that should work
	res, err := harness.ExecTask(task.ID, []string{"/usr/bin/stat", "/tmp"}, 1*time.Second)
	require.NoError(err)
	require.True(res.ExitResult.Successful())
	require.True(len(res.Stdout) > 100)

	// Exec a command that should fail
	res, err = harness.ExecTask(task.ID, []string{"/usr/bin/stat", "notarealfile123abc"}, 1*time.Second)
	require.NoError(err)
	require.False(res.ExitResult.Successful())
	require.Contains(string(res.Stdout), "No such file or directory")

	require.NoError(harness.DestroyTask(task.ID, true))
}

func encodeDriverHelper(require *require.Assertions, task *drivers.TaskConfig, taskConfig map[string]interface{}) {
	evalCtx := &hcl.EvalContext{
		Functions: hclutils.GetStdlibFuncs(),
	}
	spec, diag := hclspec.Convert(taskConfigSpec)
	require.False(diag.HasErrors())
	taskConfigCtyVal, diag := hclutils.ParseHclInterface(taskConfig, spec, evalCtx)
	require.False(diag.HasErrors())
	err := task.EncodeDriverConfig(taskConfigCtyVal)
	require.Nil(err)
}
