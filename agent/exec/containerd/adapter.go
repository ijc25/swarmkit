package containerd

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Sirupsen/logrus"
	"github.com/containerd/containerd"
	containersapi "github.com/containerd/containerd/api/services/containers"
	"github.com/containerd/containerd/api/services/execution"
	"github.com/containerd/containerd/api/types/task"
	dockermount "github.com/docker/docker/pkg/mount"
	"github.com/docker/swarmkit/agent/exec"
	"github.com/docker/swarmkit/api"
	"github.com/docker/swarmkit/api/naming"
	"github.com/docker/swarmkit/log"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pkg/errors"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
)

var (
	devNull                    *os.File
	errAdapterNotPrepared      = errors.New("container adapter not prepared")
	mountPropagationReverseMap = map[api.Mount_BindOptions_MountPropagation]string{
		api.MountPropagationPrivate:  "private",
		api.MountPropagationRPrivate: "rprivate",
		api.MountPropagationShared:   "shared",
		api.MountPropagationRShared:  "rshared",
		api.MountPropagationRSlave:   "slave",
		api.MountPropagationSlave:    "rslave",
	}
)

// containerAdapter conducts remote operations for a container. All calls
// are mostly naked calls to the client API, seeded with information from
// containerConfig.
type containerAdapter struct {
	client         *containerd.Client
	spec           *api.ContainerSpec
	secrets        exec.SecretGetter
	name           string
	image          containerd.Image // Pulled image
	container      containerd.Container
	task           containerd.Task
	deleteResponse *execution.DeleteResponse
}

func newContainerAdapter(client *containerd.Client, task *api.Task, secrets exec.SecretGetter) (*containerAdapter, error) {
	spec := task.Spec.GetContainer()
	if spec == nil {
		return nil, exec.ErrRuntimeUnsupported
	}

	return &containerAdapter{
		client:  client,
		spec:    spec,
		secrets: secrets,
		name:    naming.Task(task),
	}, nil
}

func (c *containerAdapter) pullImage(ctx context.Context) error {
	image, err := c.client.Pull(ctx, c.spec.Image, containerd.WithPullUnpack)
	if err != nil {
		return errors.Wrap(err, "pulling container image")
	}
	c.image = image

	return nil
}

func withMounts(ctx context.Context, ms []api.Mount) containerd.SpecOpts {
	sort.Sort(mounts(ms))

	return func(s *specs.Spec) error {
		for _, m := range ms {
			if !filepath.IsAbs(m.Target) {
				return errors.Errorf("mount %s is not absolute", m.Target)
			}

			switch m.Type {
			case api.MountTypeTmpfs:
				opts := []string{"noexec", "nosuid", "nodev", "rprivate"}
				if m.TmpfsOptions != nil {
					if m.TmpfsOptions.SizeBytes <= 0 {
						return errors.New("invalid tmpfs size give")
					}
					opts = append(opts, fmt.Sprintf("size=%d", m.TmpfsOptions.SizeBytes))
					opts = append(opts, fmt.Sprintf("mode=%o", m.TmpfsOptions.Mode))
				}
				if m.ReadOnly {
					opts = append(opts, "ro")
				} else {
					opts = append(opts, "rw")
				}

				opts, err := dockermount.MergeTmpfsOptions(opts)
				if err != nil {
					return err
				}

				s.Mounts = append(s.Mounts, specs.Mount{
					Destination: m.Target,
					Type:        "tmpfs",
					Source:      "tmpfs",
					Options:     opts,
				})

			case api.MountTypeVolume:
				return errors.Errorf("volume mounts not implemented, ignoring %v", m)

			case api.MountTypeBind:
				opts := []string{"rbind"}
				if m.ReadOnly {
					opts = append(opts, "ro")
				} else {
					opts = append(opts, "rw")
				}

				propagation := "rprivate"
				if m.BindOptions != nil {
					if p, ok := mountPropagationReverseMap[m.BindOptions.Propagation]; ok {
						propagation = p
					} else {
						log.G(ctx).Warningf("unknown bind mount propagation, using %q", propagation)
					}
				}
				opts = append(opts, propagation)

				s.Mounts = append(s.Mounts, specs.Mount{
					Destination: m.Target,
					Type:        "bind",
					Source:      m.Source,
					Options:     opts,
				})
			}
		}
		return nil
	}
}

func (c *containerAdapter) isPrepared() bool {
	return c.container != nil && c.task != nil
}

func (c *containerAdapter) prepare(ctx context.Context) error {
	if c.isPrepared() {
		return errors.New("adapter already prepared")
	}
	if c.image == nil {
		return errors.New("image has not been pulled")
	}

	l := log.G(ctx).WithFields(logrus.Fields{
		"ID": c.name,
	})

	specOpts := []containerd.SpecOpts{
		containerd.WithImageConfig(ctx, c.image),
		withMounts(ctx, c.spec.Mounts),
	}

	// spec.Process.Args is config.Entrypoint + config.Cmd at this
	// point from WithImageConfig above. If the ContainerSpec
	// specifies a Command then we can completely override. If it
	// does not then all we can do is append our Args and hope
	// they do not conflict.
	// TODO(ijc) Improve this
	if len(c.spec.Command) > 0 {
		args := append(c.spec.Command, c.spec.Args...)
		specOpts = append(specOpts, containerd.WithProcessArgs(args...))
	} else {
		specOpts = append(specOpts, func(s *specs.Spec) error {
			s.Process.Args = append(s.Process.Args, c.spec.Args...)
			return nil
		})
	}

	spec, err := containerd.GenerateSpec(specOpts...)
	if err != nil {
		return err
	}

	// TODO(ijc) Consider an addition to container library which
	// directly attaches stdin to /dev/null.
	if devNull == nil {
		if devNull, err = os.Open(os.DevNull); err != nil {
			return errors.Wrap(err, "opening null device")
		}
	}

	c.container, err = c.client.NewContainer(ctx, c.name,
		containerd.WithSpec(spec),
		containerd.WithNewRootFS(c.name, c.image))
	if err != nil {
		return errors.Wrap(err, "creating container")
	}

	// TODO(ijc) support ControllerLogs interface.
	io := containerd.NewIOWithTerminal(devNull, os.Stdout, os.Stderr, spec.Process.Terminal)

	c.task, err = c.container.NewTask(ctx, io)
	if err != nil {
		// Destroy the container we created above, but
		// propagate the original error.
		if err2 := c.container.Delete(ctx); err2 != nil {
			l.WithError(err2).Error("failed to delete container on prepare failure")
		}
		c.container = nil
		return errors.Wrap(err, "creating task")
	}

	return nil
}

func (c *containerAdapter) start(ctx context.Context) error {
	if !c.isPrepared() {
		return errAdapterNotPrepared
	}

	tasks := c.client.TaskService()

	_, err := tasks.Start(ctx, &execution.StartRequest{
		ContainerID: c.name,
	})
	return err
}

func (c *containerAdapter) eventStream(ctx context.Context, id string) (<-chan task.Event, <-chan error, error) {

	var (
		evtch = make(chan task.Event)
		errch = make(chan error)
	)

	return evtch, errch, nil
}

// events issues a call to the events API and returns a channel with all
// events. The stream of events can be shutdown by cancelling the context.
//
// A chan struct{} is returned that will be closed if the event processing
// fails and needs to be restarted.
func (c *containerAdapter) events(ctx context.Context, opts ...grpc.CallOption) (<-chan task.Event, <-chan struct{}, error) {
	if !c.isPrepared() {
		return nil, nil, errAdapterNotPrepared
	}

	l := log.G(ctx).WithFields(logrus.Fields{
		"ID": c.name,
	})

	// TODO(stevvooe): Move this to a single, global event dispatch. For
	// now, we create a connection per container.
	var (
		eventsq = make(chan task.Event)
		closed  = make(chan struct{})
	)

	l.Debugf("waiting on events")

	tasks := c.client.TaskService()
	cl, err := tasks.Events(ctx, &execution.EventsRequest{}, opts...)
	if err != nil {
		l.WithError(err).Errorf("failed to start event stream")
		return nil, nil, err
	}

	go func() {
		defer close(closed)

		for {
			evt, err := cl.Recv()
			if err != nil {
				l.WithError(err).Error("fatal error from events stream")
				return
			}
			if evt.ID != c.name {
				l.Debugf("Event for a different container %s", evt.ID)
				continue
			}

			select {
			case eventsq <- *evt:
			case <-ctx.Done():
				return
			}
		}
	}()

	return eventsq, closed, nil
}

func (c *containerAdapter) inspect(ctx context.Context) (task.Task, error) {
	if !c.isPrepared() {
		return task.Task{}, errAdapterNotPrepared
	}

	tasks := c.client.TaskService()
	rsp, err := tasks.Info(ctx, &execution.InfoRequest{ContainerID: c.name})
	if err != nil {
		return task.Task{}, err
	}
	return *rsp.Task, nil
}

func (c *containerAdapter) shutdown(ctx context.Context) (uint32, error) {
	if !c.isPrepared() {
		return 0, errAdapterNotPrepared
	}

	l := log.G(ctx).WithFields(logrus.Fields{
		"ID": c.name,
	})

	if c.deleteResponse == nil {
		var err error
		l.Debug("Deleting")

		tasks := c.client.TaskService()
		rsp, err := tasks.Delete(ctx, &execution.DeleteRequest{ContainerID: c.name})
		if err != nil {
			return 0, err
		}
		l.Debugf("Status=%d", rsp.ExitStatus)
		c.deleteResponse = rsp

		containers := c.client.ContainerService()
		_, err = containers.Delete(ctx, &containersapi.DeleteContainerRequest{
			ID: c.name,
		})
		if err != nil {
			l.WithError(err).Warnf("failed to delete container")
		}
	}

	return c.deleteResponse.ExitStatus, nil
}

func (c *containerAdapter) terminate(ctx context.Context) error {
	if !c.isPrepared() {
		return errAdapterNotPrepared
	}

	l := log.G(ctx).WithFields(logrus.Fields{
		"ID": c.name,
	})
	l.Debug("Terminate")
	return errors.New("terminate not implemented")
}

func (c *containerAdapter) remove(ctx context.Context) error {
	if !c.isPrepared() {
		return errAdapterNotPrepared
	}

	l := log.G(ctx).WithFields(logrus.Fields{
		"ID": c.name,
	})
	l.Debug("Remove")
	return nil
}

func isContainerCreateNameConflict(err error) bool {
	// container ".*" already exists
	splits := strings.SplitN(err.Error(), "\"", 3)
	return splits[0] == "container " && splits[2] == " already exists"
}

func isUnknownContainer(err error) bool {
	return strings.Contains(err.Error(), "container does not exist")
}

// For sort.Sort
type mounts []api.Mount

// Len returns the number of mounts. Used in sorting.
func (m mounts) Len() int {
	return len(m)
}

// Less returns true if the number of parts (a/b/c would be 3 parts) in the
// mount indexed by parameter 1 is less than that of the mount indexed by
// parameter 2. Used in sorting.
func (m mounts) Less(i, j int) bool {
	return m.parts(i) < m.parts(j)
}

// Swap swaps two items in an array of mounts. Used in sorting
func (m mounts) Swap(i, j int) {
	m[i], m[j] = m[j], m[i]
}

// parts returns the number of parts in the destination of a mount. Used in sorting.
func (m mounts) parts(i int) int {
	return strings.Count(filepath.Clean(m[i].Target), string(os.PathSeparator))
}