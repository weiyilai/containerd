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

package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	goruntime "runtime"
	"strings"
	"syscall"
	"time"

	"github.com/containerd/containerd/api/services/tasks/v1"
	"github.com/containerd/containerd/api/types"
	"github.com/containerd/containerd/api/types/runc/options"
	"github.com/containerd/errdefs"
	"github.com/containerd/errdefs/pkg/errgrpc"
	"github.com/containerd/typeurl/v2"
	digest "github.com/opencontainers/go-digest"
	is "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/diff"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/containerd/v2/pkg/protobuf"
	google_protobuf "github.com/containerd/containerd/v2/pkg/protobuf/types"
	"github.com/containerd/containerd/v2/pkg/rootfs"
	"github.com/containerd/containerd/v2/pkg/tracing"
	"github.com/containerd/containerd/v2/plugins"
)

// UnknownExitStatus is returned when containerd is unable to
// determine the exit status of a process. This can happen if the process never starts
// or if an error was encountered when obtaining the exit status, it is set to 255.
const UnknownExitStatus = 255

const (
	checkpointDateFormat = "01-02-2006-15:04:05"
	checkpointNameFormat = "containerd.io/checkpoint/%s:%s"
)

// Status returns process status and exit information
type Status struct {
	// Status of the process
	Status ProcessStatus
	// ExitStatus returned by the process
	ExitStatus uint32
	// ExitedTime is the time at which the process died
	ExitTime time.Time
}

// ProcessInfo provides platform specific process information
type ProcessInfo struct {
	// Pid is the process ID
	Pid uint32
	// Info includes additional process information
	// Info varies by platform
	Info *google_protobuf.Any
}

// ProcessStatus returns a human readable status for the Process representing its current status
type ProcessStatus string

const (
	// Running indicates the process is currently executing
	Running ProcessStatus = "running"
	// Created indicates the process has been created within containerd but the
	// user's defined process has not started
	Created ProcessStatus = "created"
	// Stopped indicates that the process has ran and exited
	Stopped ProcessStatus = "stopped"
	// Paused indicates that the process is currently paused
	Paused ProcessStatus = "paused"
	// Pausing indicates that the process is currently switching from a
	// running state into a paused state
	Pausing ProcessStatus = "pausing"
	// Unknown indicates that we could not determine the status from the runtime
	Unknown ProcessStatus = "unknown"
)

// IOCloseInfo allows specific io pipes to be closed on a process
type IOCloseInfo struct {
	Stdin bool
}

// IOCloserOpts allows the caller to set specific pipes as closed on a process
type IOCloserOpts func(*IOCloseInfo)

// WithStdinCloser closes the stdin of a process
func WithStdinCloser(r *IOCloseInfo) {
	r.Stdin = true
}

// CheckpointTaskInfo allows specific checkpoint information to be set for the task
type CheckpointTaskInfo struct {
	Name string
	// ParentCheckpoint is the digest of a parent checkpoint
	ParentCheckpoint digest.Digest
	// Options hold runtime specific settings for checkpointing a task
	Options interface{}

	runtime string
}

// Runtime name for the container
func (i *CheckpointTaskInfo) Runtime() string {
	return i.runtime
}

// CheckpointTaskOpts allows the caller to set checkpoint options
type CheckpointTaskOpts func(*CheckpointTaskInfo) error

// TaskInfo sets options for task creation
type TaskInfo struct {
	// Checkpoint is the Descriptor for an existing checkpoint that can be used
	// to restore a task's runtime and memory state
	Checkpoint *types.Descriptor
	// RootFS is a list of mounts to use as the task's root filesystem
	RootFS []mount.Mount
	// Options hold runtime specific settings for task creation
	Options interface{}
	// RuntimePath is an absolute path that can be used to overwrite path
	// to a shim runtime binary.
	RuntimePath string

	// runtime is the runtime name for the container, and cannot be changed.
	runtime string

	// runtimeOptions is the runtime options for the container, and when task options are set,
	// they will be based on the runtimeOptions.
	// https://github.com/containerd/containerd/issues/11568
	runtimeOptions typeurl.Any
}

// Runtime name for the container
func (i *TaskInfo) Runtime() string {
	return i.runtime
}

// getRuncOptions returns a reference to the runtime options for use by the task.
// If the set of options is not set by the opts passed into the NewTask creation
// this function first attempts to initialize the runtime options with a copy of the runtimeOptions,
// otherwise an empty set of options is assigned and returned
func (i *TaskInfo) getRuncOptions() (*options.Options, error) {
	if i.Options != nil {
		opts, ok := i.Options.(*options.Options)
		if !ok {
			return nil, errors.New("invalid runtime v2 options format")
		}
		return opts, nil
	}

	opts := &options.Options{}
	if i.runtimeOptions != nil && i.runtimeOptions.GetValue() != nil {
		if err := typeurl.UnmarshalTo(i.runtimeOptions, opts); err != nil {
			return nil, fmt.Errorf("failed to get runtime v2 options: %w", err)
		}
	}
	i.Options = opts
	return opts, nil
}

// Task is the executable object within containerd
type Task interface {
	Process

	// Pause suspends the execution of the task
	Pause(context.Context) error
	// Resume the execution of the task
	Resume(context.Context) error
	// Exec creates a new process inside the task
	Exec(context.Context, string, *specs.Process, cio.Creator) (Process, error)
	// Pids returns a list of system specific process ids inside the task
	Pids(context.Context) ([]ProcessInfo, error)
	// Checkpoint serializes the runtime and memory information of a task into an
	// OCI Index that can be pushed and pulled from a remote resource.
	//
	// Additional software like CRIU maybe required to checkpoint and restore tasks
	// NOTE: Checkpoint supports to dump task information to a directory, in this way,
	// an empty OCI Index will be returned.
	Checkpoint(context.Context, ...CheckpointTaskOpts) (Image, error)
	// Update modifies executing tasks with updated settings
	Update(context.Context, ...UpdateTaskOpts) error
	// LoadProcess loads a previously created exec'd process
	LoadProcess(context.Context, string, cio.Attach) (Process, error)
	// Metrics returns task metrics for runtime specific metrics
	//
	// The metric types are generic to containerd and change depending on the runtime
	// For the built in Linux runtime, github.com/containerd/cgroups.Metrics
	// are returned in protobuf format
	Metrics(context.Context) (*types.Metric, error)
	// Spec returns the current OCI specification for the task
	Spec(context.Context) (*oci.Spec, error)
}

var _ = (Task)(&task{})

type task struct {
	client *Client
	c      Container

	io  cio.IO
	id  string
	pid uint32
}

// Spec returns the current OCI specification for the task
func (t *task) Spec(ctx context.Context) (*oci.Spec, error) {
	return t.c.Spec(ctx)
}

// ID of the task
func (t *task) ID() string {
	return t.id
}

// Pid returns the pid or process id for the task
func (t *task) Pid() uint32 {
	return t.pid
}

func (t *task) Start(ctx context.Context) error {
	ctx, span := tracing.StartSpan(ctx, "task.Start",
		tracing.WithAttribute("task.id", t.ID()),
	)
	defer span.End()
	r, err := t.client.TaskService().Start(ctx, &tasks.StartRequest{
		ContainerID: t.id,
	})
	if err != nil {
		if t.io != nil {
			t.io.Cancel()
			t.io.Close()
		}
		return errgrpc.ToNative(err)
	}
	span.SetAttributes(tracing.Attribute("task.pid", r.Pid))
	t.pid = r.Pid
	return nil
}

func (t *task) Kill(ctx context.Context, s syscall.Signal, opts ...KillOpts) error {
	ctx, span := tracing.StartSpan(ctx, "task.Kill",
		tracing.WithAttribute("task.id", t.ID()),
		tracing.WithAttribute("task.pid", int(t.Pid())),
	)
	defer span.End()
	var i KillInfo
	for _, o := range opts {
		if err := o(ctx, &i); err != nil {
			return err
		}
	}

	span.SetAttributes(
		tracing.Attribute("task.exec.id", i.ExecID),
		tracing.Attribute("task.exec.killall", i.All),
	)
	_, err := t.client.TaskService().Kill(ctx, &tasks.KillRequest{
		Signal:      uint32(s),
		ContainerID: t.id,
		ExecID:      i.ExecID,
		All:         i.All,
	})
	if err != nil {
		return errgrpc.ToNative(err)
	}
	return nil
}

func (t *task) Pause(ctx context.Context) error {
	ctx, span := tracing.StartSpan(ctx, "task.Pause",
		tracing.WithAttribute("task.id", t.ID()),
	)
	defer span.End()
	_, err := t.client.TaskService().Pause(ctx, &tasks.PauseTaskRequest{
		ContainerID: t.id,
	})
	return errgrpc.ToNative(err)
}

func (t *task) Resume(ctx context.Context) error {
	ctx, span := tracing.StartSpan(ctx, "task.Resume",
		tracing.WithAttribute("task.id", t.ID()),
	)
	defer span.End()
	_, err := t.client.TaskService().Resume(ctx, &tasks.ResumeTaskRequest{
		ContainerID: t.id,
	})
	return errgrpc.ToNative(err)
}

func (t *task) Status(ctx context.Context) (Status, error) {
	r, err := t.client.TaskService().Get(ctx, &tasks.GetRequest{
		ContainerID: t.id,
	})
	if err != nil {
		return Status{}, errgrpc.ToNative(err)
	}
	status := ProcessStatus(strings.ToLower(r.Process.Status.String()))
	exitStatus := r.Process.ExitStatus
	exitTime := protobuf.FromTimestamp(r.Process.ExitedAt)

	return Status{
		Status:     status,
		ExitStatus: exitStatus,
		ExitTime:   exitTime,
	}, nil
}

func (t *task) Wait(ctx context.Context) (<-chan ExitStatus, error) {
	c := make(chan ExitStatus, 1)
	go func() {
		defer close(c)
		ctx, span := tracing.StartSpan(ctx, "task.Wait",
			tracing.WithAttribute("task.id", t.ID()),
		)
		defer span.End()
		r, err := t.client.TaskService().Wait(ctx, &tasks.WaitRequest{
			ContainerID: t.id,
		})
		if err != nil {
			c <- ExitStatus{
				code: UnknownExitStatus,
				err:  err,
			}
			return
		}
		c <- ExitStatus{
			code:     r.ExitStatus,
			exitedAt: protobuf.FromTimestamp(r.ExitedAt),
		}
	}()
	return c, nil
}

// Delete deletes the task and its runtime state
// it returns the exit status of the task and any errors that were encountered
// during cleanup
func (t *task) Delete(ctx context.Context, opts ...ProcessDeleteOpts) (*ExitStatus, error) {
	ctx, span := tracing.StartSpan(ctx, "task.Delete",
		tracing.WithAttribute("task.id", t.ID()),
	)
	defer span.End()
	for _, o := range opts {
		if err := o(ctx, t); err != nil {
			return nil, err
		}
	}
	status, err := t.Status(ctx)
	if err != nil && errdefs.IsNotFound(err) {
		return nil, err
	}

	runtime, err := t.client.defaultRuntime(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get default runtime: %w", err)
	}

	switch status.Status {
	case Stopped, Unknown, "":
	case Created:
		if runtime == plugins.RuntimePlugin.String()+".windows" {
			// On windows Created is akin to Stopped
			break
		}
		if t.pid == 0 {
			// allow for deletion of created tasks with PID 0
			// https://github.com/containerd/containerd/issues/7357
			break
		}
		fallthrough
	default:
		return nil, fmt.Errorf("task must be stopped before deletion: %s: %w", status.Status, errdefs.ErrFailedPrecondition)
	}
	if t.io != nil {
		// io.Wait locks for restored tasks on Windows unless we call
		// io.Close first (https://github.com/containerd/containerd/issues/5621)
		// in other cases, preserve the contract and let IO finish before closing
		if runtime == plugins.RuntimePlugin.String()+".windows" {
			t.io.Close()
		}
		// io.Cancel is used to cancel the io goroutine while it is in
		// fifo-opening state. It does not stop the pipes since these
		// should be closed on the shim's side, otherwise we might lose
		// data from the container!
		t.io.Cancel()
		t.io.Wait()
	}
	r, err := t.client.TaskService().Delete(ctx, &tasks.DeleteTaskRequest{
		ContainerID: t.id,
	})
	if err != nil {
		return nil, errgrpc.ToNative(err)
	}
	// Only cleanup the IO after a successful Delete
	if t.io != nil {
		t.io.Close()
	}
	return &ExitStatus{code: r.ExitStatus, exitedAt: protobuf.FromTimestamp(r.ExitedAt)}, nil
}

func (t *task) Exec(ctx context.Context, id string, spec *specs.Process, ioCreate cio.Creator) (_ Process, retErr error) {
	ctx, span := tracing.StartSpan(ctx, "task.Exec",
		tracing.WithAttribute("task.id", t.ID()),
	)
	defer span.End()
	if id == "" {
		return nil, fmt.Errorf("exec id must not be empty: %w", errdefs.ErrInvalidArgument)
	}
	span.SetAttributes(tracing.Attribute("task.exec.id", id))
	i, err := ioCreate(id)
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil && i != nil {
			i.Cancel()
			i.Close()
		}
	}()
	pSpec, err := typeurl.MarshalAnyToProto(spec)
	if err != nil {
		return nil, err
	}
	cfg := i.Config()
	request := &tasks.ExecProcessRequest{
		ContainerID: t.id,
		ExecID:      id,
		Terminal:    cfg.Terminal,
		Stdin:       cfg.Stdin,
		Stdout:      cfg.Stdout,
		Stderr:      cfg.Stderr,
		Spec:        pSpec,
	}
	if _, err := t.client.TaskService().Exec(ctx, request); err != nil {
		i.Cancel()
		i.Wait()
		i.Close()
		return nil, errgrpc.ToNative(err)
	}
	return &process{
		id:   id,
		task: t,
		io:   i,
	}, nil
}

func (t *task) Pids(ctx context.Context) ([]ProcessInfo, error) {
	response, err := t.client.TaskService().ListPids(ctx, &tasks.ListPidsRequest{
		ContainerID: t.id,
	})
	if err != nil {
		return nil, errgrpc.ToNative(err)
	}
	var processList []ProcessInfo
	for _, p := range response.Processes {
		processList = append(processList, ProcessInfo{
			Pid:  p.Pid,
			Info: p.Info,
		})
	}
	return processList, nil
}

func (t *task) CloseIO(ctx context.Context, opts ...IOCloserOpts) error {
	ctx, span := tracing.StartSpan(ctx, "task.CloseIO",
		tracing.WithAttribute("task.id", t.ID()),
	)
	defer span.End()
	r := &tasks.CloseIORequest{
		ContainerID: t.id,
	}
	var i IOCloseInfo
	for _, o := range opts {
		o(&i)
	}
	r.Stdin = i.Stdin

	_, err := t.client.TaskService().CloseIO(ctx, r)
	return errgrpc.ToNative(err)
}

func (t *task) IO() cio.IO {
	return t.io
}

func (t *task) Resize(ctx context.Context, w, h uint32) error {
	ctx, span := tracing.StartSpan(ctx, "task.Resize",
		tracing.WithAttribute("task.id", t.ID()),
	)
	defer span.End()
	_, err := t.client.TaskService().ResizePty(ctx, &tasks.ResizePtyRequest{
		ContainerID: t.id,
		Width:       w,
		Height:      h,
	})
	return errgrpc.ToNative(err)
}

// NOTE: Checkpoint supports to dump task information to a directory, in this way, an empty
// OCI Index will be returned.
func (t *task) Checkpoint(ctx context.Context, opts ...CheckpointTaskOpts) (Image, error) {
	ctx, done, err := t.client.WithLease(ctx)
	if err != nil {
		return nil, err
	}
	defer done(ctx)
	cr, err := t.client.ContainerService().Get(ctx, t.id)
	if err != nil {
		return nil, err
	}

	request := &tasks.CheckpointTaskRequest{
		ContainerID: t.id,
	}
	i := CheckpointTaskInfo{
		runtime: cr.Runtime.Name,
	}
	for _, o := range opts {
		if err := o(&i); err != nil {
			return nil, err
		}
	}
	// set a default name
	if i.Name == "" {
		i.Name = fmt.Sprintf(checkpointNameFormat, t.id, time.Now().Format(checkpointDateFormat))
	}
	request.ParentCheckpoint = i.ParentCheckpoint.String()
	if i.Options != nil {
		o, err := typeurl.MarshalAnyToProto(i.Options)
		if err != nil {
			return nil, err
		}
		request.Options = o
	}

	status, err := t.Status(ctx)
	if err != nil {
		return nil, err
	}

	if status.Status != Paused {
		// make sure we pause it and resume after all other filesystem operations are completed
		if err := t.Pause(ctx); err != nil {
			return nil, err
		}
		defer t.Resume(ctx)
	}

	index := v1.Index{
		Versioned: is.Versioned{
			SchemaVersion: 2,
		},
		Annotations: make(map[string]string),
	}
	if err := t.checkpointTask(ctx, &index, request); err != nil {
		return nil, err
	}
	// if checkpoint image path passed, jump checkpoint image,
	// return an empty image
	if isCheckpointPathExist(cr.Runtime.Name, i.Options) {
		return NewImage(t.client, images.Image{}), nil
	}

	if cr.Image != "" {
		if err := t.checkpointImage(ctx, &index, cr.Image); err != nil {
			return nil, err
		}
		index.Annotations["image.name"] = cr.Image
	}
	if cr.SnapshotKey != "" {
		if err := t.checkpointRWSnapshot(ctx, &index, cr.Snapshotter, cr.SnapshotKey); err != nil {
			return nil, err
		}
	}
	desc, err := writeIndex(ctx, &index, t.client, t.id)
	if err != nil {
		return nil, err
	}
	im := images.Image{
		Name:   i.Name,
		Target: desc,
		Labels: map[string]string{
			"containerd.io/checkpoint": "true",
		},
	}
	if im, err = t.client.ImageService().Create(ctx, im); err != nil {
		return nil, err
	}
	return NewImage(t.client, im), nil
}

// UpdateTaskInfo allows updated specific settings to be changed on a task
type UpdateTaskInfo struct {
	// Resources updates a tasks resource constraints
	Resources interface{}
	// Annotations allows arbitrary and/or experimental resource constraints for task update
	Annotations map[string]string
}

// UpdateTaskOpts allows a caller to update task settings
type UpdateTaskOpts func(context.Context, *Client, *UpdateTaskInfo) error

func (t *task) Update(ctx context.Context, opts ...UpdateTaskOpts) error {
	ctx, span := tracing.StartSpan(ctx, "task.Update",
		tracing.WithAttribute("task.id", t.ID()),
	)
	defer span.End()
	request := &tasks.UpdateTaskRequest{
		ContainerID: t.id,
	}
	var i UpdateTaskInfo
	for _, o := range opts {
		if err := o(ctx, t.client, &i); err != nil {
			return err
		}
	}
	if i.Resources != nil {
		r, err := typeurl.MarshalAny(i.Resources)
		if err != nil {
			return err
		}
		request.Resources = typeurl.MarshalProto(r)
	}
	if i.Annotations != nil {
		request.Annotations = i.Annotations
	}
	_, err := t.client.TaskService().Update(ctx, request)
	return errgrpc.ToNative(err)
}

func (t *task) LoadProcess(ctx context.Context, id string, ioAttach cio.Attach) (Process, error) {
	if id == t.id && ioAttach == nil {
		return t, nil
	}
	response, err := t.client.TaskService().Get(ctx, &tasks.GetRequest{
		ContainerID: t.id,
		ExecID:      id,
	})
	if err != nil {
		err = errgrpc.ToNative(err)
		if errdefs.IsNotFound(err) {
			return nil, fmt.Errorf("no running process found: %w", err)
		}
		return nil, err
	}
	var i cio.IO
	if ioAttach != nil {
		if i, err = attachExistingIO(response, ioAttach); err != nil {
			return nil, err
		}
	}
	return &process{
		id:   id,
		task: t,
		io:   i,
	}, nil
}

func (t *task) Metrics(ctx context.Context) (*types.Metric, error) {
	response, err := t.client.TaskService().Metrics(ctx, &tasks.MetricsRequest{
		Filters: []string{
			"id==" + t.id,
		},
	})
	if err != nil {
		return nil, errgrpc.ToNative(err)
	}

	if response.Metrics == nil {
		_, err := t.Status(ctx)
		if err != nil && errdefs.IsNotFound(err) {
			return nil, err
		}
		return nil, errors.New("no metrics received")
	}

	return response.Metrics[0], nil
}

func (t *task) checkpointTask(ctx context.Context, index *v1.Index, request *tasks.CheckpointTaskRequest) error {
	response, err := t.client.TaskService().Checkpoint(ctx, request)
	if err != nil {
		return errgrpc.ToNative(err)
	}
	// NOTE: response.Descriptors can be an empty slice if checkpoint image is jumped
	// add the checkpoint descriptors to the index
	for _, d := range response.Descriptors {
		index.Manifests = append(index.Manifests, v1.Descriptor{
			MediaType: d.MediaType,
			Size:      d.Size,
			Digest:    digest.Digest(d.Digest),
			Platform: &v1.Platform{
				OS:           goruntime.GOOS,
				Architecture: goruntime.GOARCH,
			},
			Annotations: d.Annotations,
		})
	}
	return nil
}

func (t *task) checkpointRWSnapshot(ctx context.Context, index *v1.Index, snapshotterName string, id string) error {
	opts := []diff.Opt{
		diff.WithReference(fmt.Sprintf("checkpoint-rw-%s", id)),
	}
	rw, err := rootfs.CreateDiff(ctx, id, t.client.SnapshotService(snapshotterName), t.client.DiffService(), opts...)
	if err != nil {
		return err
	}
	rw.Platform = &v1.Platform{
		OS:           goruntime.GOOS,
		Architecture: goruntime.GOARCH,
	}
	index.Manifests = append(index.Manifests, rw)
	return nil
}

func (t *task) checkpointImage(ctx context.Context, index *v1.Index, image string) error {
	if image == "" {
		return fmt.Errorf("cannot checkpoint image with empty name")
	}
	ir, err := t.client.ImageService().Get(ctx, image)
	if err != nil {
		return err
	}
	index.Manifests = append(index.Manifests, ir.Target)
	return nil
}

func writeContent(ctx context.Context, store content.Ingester, mediaType, ref string, r io.Reader, opts ...content.Opt) (d v1.Descriptor, err error) {
	writer, err := store.Writer(ctx, content.WithRef(ref))
	if err != nil {
		return d, err
	}
	defer writer.Close()
	size, err := io.Copy(writer, r)
	if err != nil {
		return d, err
	}

	if err := writer.Commit(ctx, size, "", opts...); err != nil {
		if !errdefs.IsAlreadyExists(err) {
			return d, err
		}
	}
	return v1.Descriptor{
		MediaType: mediaType,
		Digest:    writer.Digest(),
		Size:      size,
	}, nil
}

// isCheckpointPathExist only suitable for runc runtime now
func isCheckpointPathExist(runtime string, v interface{}) bool {
	if v == nil {
		return false
	}

	switch runtime {
	case plugins.RuntimeRuncV2:
		if opts, ok := v.(*options.CheckpointOptions); ok && opts.ImagePath != "" {
			return true
		}
	}

	return false
}
