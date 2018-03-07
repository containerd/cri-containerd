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

package containerd

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strconv"

	eventsapi "github.com/containerd/containerd/api/services/events/v1"
	introspectionapi "github.com/containerd/containerd/api/services/introspection/v1"
	leasesapi "github.com/containerd/containerd/api/services/leases/v1"
	"github.com/containerd/containerd/api/services/tasks/v1"
	versionservice "github.com/containerd/containerd/api/services/version/v1"
	"github.com/containerd/containerd/containers"
	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/platforms"
	"github.com/containerd/containerd/plugin"
	"github.com/containerd/containerd/remotes"
	"github.com/containerd/containerd/remotes/docker"
	"github.com/containerd/containerd/remotes/docker/schema1"
	"github.com/containerd/containerd/snapshots"
	"github.com/containerd/typeurl"
	ptypes "github.com/gogo/protobuf/types"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pkg/errors"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health/grpc_health_v1"
)

func init() {
	const prefix = "types.containerd.io"
	// register TypeUrls for commonly marshaled external types
	major := strconv.Itoa(specs.VersionMajor)
	typeurl.Register(&specs.Spec{}, prefix, "opencontainers/runtime-spec", major, "Spec")
	typeurl.Register(&specs.Process{}, prefix, "opencontainers/runtime-spec", major, "Process")
	typeurl.Register(&specs.LinuxResources{}, prefix, "opencontainers/runtime-spec", major, "LinuxResources")
	typeurl.Register(&specs.WindowsResources{}, prefix, "opencontainers/runtime-spec", major, "WindowsResources")
}

// New returns a new containerd client that is connected to the containerd
// instance provided by address
func New(address string, opts ...ClientOpt) (*Client, error) {
	var copts clientOpts
	for _, o := range opts {
		if err := o(&copts); err != nil {
			return nil, err
		}
	}
	services, err := newGRPCServices(address, copts)
	if err != nil {
		return nil, err
	}
	return &Client{
		Services: services,
		runtime:  fmt.Sprintf("%s.%s", plugin.RuntimePlugin, runtime.GOOS),
	}, nil
}

// NewWithServices returns a new containerd client that communicates with the
// containerd instance through existing services.
func NewWithServices(services Services, opts ...ClientOpt) (*Client, error) {
	return &Client{
		Services: services,
		runtime:  fmt.Sprintf("%s.%s", plugin.RuntimePlugin, runtime.GOOS),
	}, nil
}

// NewWithConn returns a new containerd client that is connected to the containerd
// instance provided by the connection
func NewWithConn(conn *grpc.ClientConn, opts ...ClientOpt) (*Client, error) {
	services, err := newGRPCServicesWithConn(conn)
	if err != nil {
		return nil, err
	}
	return &Client{
		Services: services,
		runtime:  fmt.Sprintf("%s.%s", plugin.RuntimePlugin, runtime.GOOS),
	}, nil
}

// Services are all required underlying services by Client.
type Services interface {
	// Reconnect re-establishes the connection to the containerd services
	Reconnect() error
	// Close closes the connection to the containerd services
	Close() error
	// NamespaceService returns the underlying Namespaces Store
	NamespaceService() namespaces.Store
	// ContainerService returns the underlying container Store
	ContainerService() containers.Store
	// ContentStore returns the underlying content Store
	ContentStore() content.Store
	// SnapshotService returns the underlying snapshotter for the provided snapshotter name
	SnapshotService(snapshotterName string) snapshots.Snapshotter
	// TaskService returns the underlying TasksClient
	TaskService() tasks.TasksClient
	// ImageService returns the underlying image Store
	ImageService() images.Store
	// DiffService returns the underlying Differ
	DiffService() DiffService
	// IntrospectionService returns the underlying Introspection Client
	IntrospectionService() introspectionapi.IntrospectionClient
	// LeasesService returns the underlying Leases Client
	LeasesService() leasesapi.LeasesClient
	// HealthService returns the underlying GRPC HealthClient
	HealthService() grpc_health_v1.HealthClient
	// EventService returns the underlying EventsClient
	EventService() eventsapi.EventsClient
	// VersionService returns the underlying VersionClient
	VersionService() versionservice.VersionClient
}

// Client is the client to interact with containerd and its various services
// using a uniform interface
type Client struct {
	Services
	runtime string
}

// IsServing returns true if the client can successfully connect to the
// containerd daemon and the healthcheck service returns the SERVING
// response.
// This call will block if a transient error is encountered during
// connection. A timeout can be set in the context to ensure it returns
// early.
func (c *Client) IsServing(ctx context.Context) (bool, error) {
	r, err := c.HealthService().Check(ctx, &grpc_health_v1.HealthCheckRequest{}, grpc.FailFast(false))
	if err != nil {
		return false, err
	}
	return r.Status == grpc_health_v1.HealthCheckResponse_SERVING, nil
}

// Containers returns all containers created in containerd
func (c *Client) Containers(ctx context.Context, filters ...string) ([]Container, error) {
	r, err := c.ContainerService().List(ctx, filters...)
	if err != nil {
		return nil, err
	}
	var out []Container
	for _, container := range r {
		out = append(out, containerFromRecord(c, container))
	}
	return out, nil
}

// NewContainer will create a new container in container with the provided id
// the id must be unique within the namespace
func (c *Client) NewContainer(ctx context.Context, id string, opts ...NewContainerOpts) (Container, error) {
	ctx, done, err := c.WithLease(ctx)
	if err != nil {
		return nil, err
	}
	defer done()

	container := containers.Container{
		ID: id,
		Runtime: containers.RuntimeInfo{
			Name: c.runtime,
		},
	}
	for _, o := range opts {
		if err := o(ctx, c, &container); err != nil {
			return nil, err
		}
	}
	r, err := c.ContainerService().Create(ctx, container)
	if err != nil {
		return nil, err
	}
	return containerFromRecord(c, r), nil
}

// LoadContainer loads an existing container from metadata
func (c *Client) LoadContainer(ctx context.Context, id string) (Container, error) {
	r, err := c.ContainerService().Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return containerFromRecord(c, r), nil
}

// RemoteContext is used to configure object resolutions and transfers with
// remote content stores and image providers.
type RemoteContext struct {
	// Resolver is used to resolve names to objects, fetchers, and pushers.
	// If no resolver is provided, defaults to Docker registry resolver.
	Resolver remotes.Resolver

	// Unpack is done after an image is pulled to extract into a snapshotter.
	// If an image is not unpacked on pull, it can be unpacked any time
	// afterwards. Unpacking is required to run an image.
	Unpack bool

	// Snapshotter used for unpacking
	Snapshotter string

	// Labels to be applied to the created image
	Labels map[string]string

	// BaseHandlers are a set of handlers which get are called on dispatch.
	// These handlers always get called before any operation specific
	// handlers.
	BaseHandlers []images.Handler

	// ConvertSchema1 is whether to convert Docker registry schema 1
	// manifests. If this option is false then any image which resolves
	// to schema 1 will return an error since schema 1 is not supported.
	ConvertSchema1 bool
}

func defaultRemoteContext() *RemoteContext {
	return &RemoteContext{
		Resolver: docker.NewResolver(docker.ResolverOptions{
			Client: http.DefaultClient,
		}),
		Snapshotter: DefaultSnapshotter,
	}
}

// Pull downloads the provided content into containerd's content store
func (c *Client) Pull(ctx context.Context, ref string, opts ...RemoteOpt) (Image, error) {
	pullCtx := defaultRemoteContext()
	for _, o := range opts {
		if err := o(c, pullCtx); err != nil {
			return nil, err
		}
	}
	store := c.ContentStore()

	ctx, done, err := c.WithLease(ctx)
	if err != nil {
		return nil, err
	}
	defer done()

	name, desc, err := pullCtx.Resolver.Resolve(ctx, ref)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to resolve reference %q", ref)
	}
	fetcher, err := pullCtx.Resolver.Fetcher(ctx, name)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get fetcher for %q", name)
	}

	var (
		schema1Converter *schema1.Converter
		handler          images.Handler
	)
	if desc.MediaType == images.MediaTypeDockerSchema1Manifest && pullCtx.ConvertSchema1 {
		schema1Converter = schema1.NewConverter(store, fetcher)
		handler = images.Handlers(append(pullCtx.BaseHandlers, schema1Converter)...)
	} else {
		// Get all the children for a descriptor
		childrenHandler := images.ChildrenHandler(store)
		// Set any children labels for that content
		childrenHandler = images.SetChildrenLabels(store, childrenHandler)
		// Filter the childen by the platform
		childrenHandler = images.FilterPlatform(platforms.Default(), childrenHandler)

		handler = images.Handlers(append(pullCtx.BaseHandlers,
			remotes.FetchHandler(store, fetcher),
			childrenHandler,
		)...)
	}

	if err := images.Dispatch(ctx, handler, desc); err != nil {
		return nil, err
	}
	if schema1Converter != nil {
		desc, err = schema1Converter.Convert(ctx)
		if err != nil {
			return nil, err
		}
	}

	imgrec := images.Image{
		Name:   name,
		Target: desc,
		Labels: pullCtx.Labels,
	}

	is := c.ImageService()
	if created, err := is.Create(ctx, imgrec); err != nil {
		if !errdefs.IsAlreadyExists(err) {
			return nil, err
		}

		updated, err := is.Update(ctx, imgrec)
		if err != nil {
			return nil, err
		}

		imgrec = updated
	} else {
		imgrec = created
	}

	img := &image{
		client: c,
		i:      imgrec,
	}
	if pullCtx.Unpack {
		if err := img.Unpack(ctx, pullCtx.Snapshotter); err != nil {
			errors.Wrapf(err, "failed to unpack image on snapshotter %s", pullCtx.Snapshotter)
		}
	}
	return img, nil
}

// Push uploads the provided content to a remote resource
func (c *Client) Push(ctx context.Context, ref string, desc ocispec.Descriptor, opts ...RemoteOpt) error {
	pushCtx := defaultRemoteContext()
	for _, o := range opts {
		if err := o(c, pushCtx); err != nil {
			return err
		}
	}

	pusher, err := pushCtx.Resolver.Pusher(ctx, ref)
	if err != nil {
		return err
	}

	return remotes.PushContent(ctx, pusher, desc, c.ContentStore(), pushCtx.BaseHandlers...)
}

// GetImage returns an existing image
func (c *Client) GetImage(ctx context.Context, ref string) (Image, error) {
	i, err := c.ImageService().Get(ctx, ref)
	if err != nil {
		return nil, err
	}
	return &image{
		client: c,
		i:      i,
	}, nil
}

// ListImages returns all existing images
func (c *Client) ListImages(ctx context.Context, filters ...string) ([]Image, error) {
	imgs, err := c.ImageService().List(ctx, filters...)
	if err != nil {
		return nil, err
	}
	images := make([]Image, len(imgs))
	for i, img := range imgs {
		images[i] = &image{
			client: c,
			i:      img,
		}
	}
	return images, nil
}

// Subscribe to events that match one or more of the provided filters.
//
// Callers should listen on both the envelope and errs channels. If the errs
// channel returns nil or an error, the subscriber should terminate.
//
// The subscriber can stop receiving events by canceling the provided context.
// The errs channel will be closed and return a nil error.
func (c *Client) Subscribe(ctx context.Context, filters ...string) (ch <-chan *eventsapi.Envelope, errs <-chan error) {
	var (
		evq  = make(chan *eventsapi.Envelope)
		errq = make(chan error, 1)
	)

	errs = errq
	ch = evq

	session, err := c.EventService().Subscribe(ctx, &eventsapi.SubscribeRequest{
		Filters: filters,
	})
	if err != nil {
		errq <- err
		close(errq)
		return
	}

	go func() {
		defer close(errq)

		for {
			ev, err := session.Recv()
			if err != nil {
				errq <- err
				return
			}

			select {
			case evq <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()

	return ch, errs
}

// Version of containerd
type Version struct {
	// Version number
	Version string
	// Revision from git that was built
	Revision string
}

// Version returns the version of containerd that the client is connected to
func (c *Client) Version(ctx context.Context) (Version, error) {
	response, err := c.VersionService().Version(ctx, &ptypes.Empty{})
	if err != nil {
		return Version{}, err
	}
	return Version{
		Version:  response.Version,
		Revision: response.Revision,
	}, nil
}

type importOpts struct {
}

// ImportOpt allows the caller to specify import specific options
type ImportOpt func(c *importOpts) error

func resolveImportOpt(opts ...ImportOpt) (importOpts, error) {
	var iopts importOpts
	for _, o := range opts {
		if err := o(&iopts); err != nil {
			return iopts, err
		}
	}
	return iopts, nil
}

// Import imports an image from a Tar stream using reader.
// Caller needs to specify importer. Future version may use oci.v1 as the default.
// Note that unreferrenced blobs may be imported to the content store as well.
func (c *Client) Import(ctx context.Context, importer images.Importer, reader io.Reader, opts ...ImportOpt) ([]Image, error) {
	_, err := resolveImportOpt(opts...) // unused now
	if err != nil {
		return nil, err
	}

	ctx, done, err := c.WithLease(ctx)
	if err != nil {
		return nil, err
	}
	defer done()

	imgrecs, err := importer.Import(ctx, c.ContentStore(), reader)
	if err != nil {
		// is.Update() is not called on error
		return nil, err
	}

	is := c.ImageService()
	var images []Image
	for _, imgrec := range imgrecs {
		if updated, err := is.Update(ctx, imgrec, "target"); err != nil {
			if !errdefs.IsNotFound(err) {
				return nil, err
			}

			created, err := is.Create(ctx, imgrec)
			if err != nil {
				return nil, err
			}

			imgrec = created
		} else {
			imgrec = updated
		}

		images = append(images, &image{
			client: c,
			i:      imgrec,
		})
	}
	return images, nil
}

type exportOpts struct {
}

// ExportOpt allows the caller to specify export-specific options
type ExportOpt func(c *exportOpts) error

func resolveExportOpt(opts ...ExportOpt) (exportOpts, error) {
	var eopts exportOpts
	for _, o := range opts {
		if err := o(&eopts); err != nil {
			return eopts, err
		}
	}
	return eopts, nil
}

// Export exports an image to a Tar stream.
// OCI format is used by default.
// It is up to caller to put "org.opencontainers.image.ref.name" annotation to desc.
// TODO(AkihiroSuda): support exporting multiple descriptors at once to a single archive stream.
func (c *Client) Export(ctx context.Context, exporter images.Exporter, desc ocispec.Descriptor, opts ...ExportOpt) (io.ReadCloser, error) {
	_, err := resolveExportOpt(opts...) // unused now
	if err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(exporter.Export(ctx, c.ContentStore(), desc, pw))
	}()
	return pr, nil
}
