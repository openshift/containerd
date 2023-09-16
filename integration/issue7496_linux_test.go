/*
   Copyright The containerd Authors.

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

package integration

import (
	"bufio"
	"context"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/runtime/v2/shim"
	apitask "github.com/containerd/containerd/runtime/v2/task"
	"github.com/containerd/ttrpc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	exec "golang.org/x/sys/execabs"
)

// TestIssue7496 is used to reproduce https://github.com/containerd/containerd/issues/7496
//
// NOTE: https://github.com/containerd/containerd/issues/8931 is the same issue.
func TestIssue7496(t *testing.T) {
	t.Logf("Checking CRI config's default runtime")
	criCfg, err := CRIConfig()
	require.NoError(t, err)

	typ := criCfg.ContainerdConfig.Runtimes[criCfg.ContainerdConfig.DefaultRuntimeName].Type
	if !strings.HasSuffix(typ, "runc.v2") {
		t.Skipf("default runtime should be runc.v2, but it's not: %s", typ)
	}

	ctx := namespaces.WithNamespace(context.Background(), "k8s.io")

	t.Logf("Create a pod config and run sandbox container")
	sbConfig := PodSandboxConfig("sandbox", "issue7496")
	sbID, err := runtimeService.RunPodSandbox(sbConfig, *runtimeHandler)
	require.NoError(t, err)

	shimCli := connectToShim(ctx, t, sbID)

	delayInSec := 12
	t.Logf("[shim pid: %d]: Injecting %d seconds delay to umount2 syscall",
		shimPid(ctx, t, shimCli),
		delayInSec)

	doneCh := injectDelayToUmount2(ctx, t, shimCli, delayInSec /* CRI plugin uses 10 seconds to delete task */)

	t.Logf("Create a container config and run container in a pod")
	pauseImage := GetImage(Pause)
	EnsureImageExists(t, pauseImage)

	containerConfig := ContainerConfig("pausecontainer", pauseImage)
	cnID, err := runtimeService.CreateContainer(sbID, containerConfig, sbConfig)
	require.NoError(t, err)
	require.NoError(t, runtimeService.StartContainer(cnID))

	t.Logf("Start to StopPodSandbox and RemovePodSandbox")
	ctx, cancelFn := context.WithTimeout(ctx, 3*time.Minute)
	defer cancelFn()
	for {
		select {
		case <-ctx.Done():
			require.NoError(t, ctx.Err(), "The StopPodSandbox should be done in time")
		default:
		}

		err := runtimeService.StopPodSandbox(sbID)
		if err != nil {
			t.Logf("Failed to StopPodSandbox: %v", err)
			continue
		}

		err = runtimeService.RemovePodSandbox(sbID)
		if err == nil {
			break
		}
		t.Logf("Failed to RemovePodSandbox: %v", err)
		time.Sleep(1 * time.Second)
	}

	t.Logf("PodSandbox %s has been deleted and start to wait for strace exit", sbID)
	select {
	case <-time.After(15 * time.Second):
		resp, err := shimCli.Connect(ctx, &apitask.ConnectRequest{})
		assert.Error(t, err, "should failed to call shim connect API")

		t.Errorf("Strace doesn't exit in time")

		t.Logf("Cleanup the shim (pid: %d)", resp.ShimPid)
		syscall.Kill(int(resp.ShimPid), syscall.SIGKILL)
		<-doneCh
	case <-doneCh:
	}
}

// injectDelayToUmount2 uses strace(1) to inject delay on umount2 syscall to
// simulate IO pressure because umount2 might force kernel to syncfs, for
// example, umount overlayfs rootfs which doesn't with volatile.
//
// REF: https://man7.org/linux/man-pages/man1/strace.1.html
func injectDelayToUmount2(ctx context.Context, t *testing.T, shimCli apitask.TaskService, delayInSec int) chan struct{} {
	pid := shimPid(ctx, t, shimCli)

	doneCh := make(chan struct{})

	cmd := exec.CommandContext(ctx, "strace",
		"-p", strconv.Itoa(int(pid)), "-f", // attach to all the threads
		"--detach-on=execve", // stop to attach runc child-processes
		"--trace=umount2",    // only trace umount2 syscall
		"-e", "inject=umount2:delay_enter="+strconv.Itoa(delayInSec)+"s",
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}

	pipeR, pipeW := io.Pipe()
	cmd.Stdout = pipeW
	cmd.Stderr = pipeW

	require.NoError(t, cmd.Start())

	// ensure that strace has attached to the shim
	readyCh := make(chan struct{})
	go func() {
		defer close(doneCh)

		bufReader := bufio.NewReader(pipeR)
		_, err := bufReader.Peek(1)
		assert.NoError(t, err, "failed to ensure that strace has attached to shim")

		close(readyCh)
		io.Copy(os.Stdout, bufReader)
		t.Logf("Strace has exited")
	}()

	go func() {
		defer pipeW.Close()
		assert.NoError(t, cmd.Wait(), "strace should exit with zero code")
	}()

	<-readyCh
	return doneCh
}

func connectToShim(ctx context.Context, t *testing.T, id string) apitask.TaskService {
	addr, err := shim.SocketAddress(ctx, containerdEndpoint, id)
	require.NoError(t, err)
	addr = strings.TrimPrefix(addr, "unix://")

	conn, err := net.Dial("unix", addr)
	require.NoError(t, err)

	client := ttrpc.NewClient(conn)
	return apitask.NewTaskClient(client)
}

func shimPid(ctx context.Context, t *testing.T, shimCli apitask.TaskService) uint32 {
	resp, err := shimCli.Connect(ctx, &apitask.ConnectRequest{})
	require.NoError(t, err)
	return resp.ShimPid
}
