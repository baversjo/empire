package github

import (
	"io"

	"github.com/ejholmes/hookshot/events"
	"github.com/remind101/empire"
	"github.com/remind101/empire/pkg/dockerutil"
	"github.com/remind101/pkg/trace"
	"github.com/remind101/tugboat"
	"golang.org/x/net/context"
)

// deployer represents something that can deploy a github deployment.
type deployer interface {
	// Deploy performs the deployment, writing output to w.
	Deploy(context.Context, events.Deployment, io.Writer) error
}

type deployerFunc func(context.Context, events.Deployment, io.Writer) error

func (fn deployerFunc) Deploy(ctx context.Context, event events.Deployment, w io.Writer) error {
	return fn(ctx, event, w)
}

// newDeployer is a factory method that generates a composed deployer instance
// depending on the options.
func newDeployer(e *empire.Empire, opts Options) deployer {
	var d deployer
	d = newEmpireDeployer(e, opts.ImageTemplate)

	if opts.TugboatURL != "" {
		d = newTugboatDeployer(d, opts.TugboatURL)
	}

	return &asyncDeployer{
		deployer: &tracedDeployer{deployer: d},
	}
}

// Empire mocks the Empire interface we use.
type Empire interface {
	Deploy(context.Context, empire.DeploymentsCreateOpts) (*empire.Release, error)
}

// empireDeployer is a deployer implementation that uses the Deploy method in
// Empire to perform the deployment.
type empireDeployer struct {
	Empire
	imageTmpl string
}

// newEmpireDeployer returns a new empireDeployer instance.
func newEmpireDeployer(e *empire.Empire, imageTmpl string) *empireDeployer {
	return &empireDeployer{
		Empire:    e,
		imageTmpl: imageTmpl,
	}
}

func (d *empireDeployer) Deploy(ctx context.Context, p events.Deployment, w io.Writer) error {
	img, err := Image(d.imageTmpl, p)
	if err != nil {
		return err
	}

	_, err = d.Empire.Deploy(ctx, empire.DeploymentsCreateOpts{
		Image:  img,
		Output: w,
		User:   &empire.User{Name: p.Deployment.Creator.Login},
	})

	return err
}

// tugboatDeployer is an implementtion of the deployer interface that sends logs
// and updates the status of the deployment within a Tugboat instance.
type tugboatDeployer struct {
	deployer
	client *tugboat.Client
}

func newTugboatDeployer(d deployer, url string) *tugboatDeployer {
	c := tugboat.NewClient(nil)
	c.URL = url
	return &tugboatDeployer{
		deployer: d,
		client:   c,
	}
}

func (d *tugboatDeployer) Deploy(ctx context.Context, event events.Deployment, out io.Writer) error {
	opts := tugboat.NewDeployOptsFromEvent(event)

	// Perform the deployment, wrapped in Deploy. This will automatically
	// write hte logs to tugboat and update the deployment status when this
	// function returns.
	_, err := d.client.Deploy(ctx, opts, provider(func(ctx context.Context, _ *tugboat.Deployment, w io.Writer) error {
		// What we send to tugboat should be a plain text stream.
		p := dockerutil.DecodeJSONMessageStream(w)

		// Write logs to both tugboat as well as the writer we were
		// provided (probably stdout).
		w = io.MultiWriter(p, out)

		if err := d.deployer.Deploy(ctx, event, w); err != nil {
			return err
		}

		if err := p.Err(); err != nil {
			return err
		}

		return nil
	}))

	return err
}

// provider implements the tugboat.Provider interface.
type provider func(context.Context, *tugboat.Deployment, io.Writer) error

func (fn provider) Name() string {
	return "empire"
}

func (fn provider) Deploy(ctx context.Context, d *tugboat.Deployment, w io.Writer) error {
	return fn(ctx, d, w)
}

// tracedDeployer is an implementation of the deployer interface that calls
// trace.Trace.
type tracedDeployer struct {
	deployer
}

func (d *tracedDeployer) Deploy(ctx context.Context, p events.Deployment, w io.Writer) (err error) {
	ctx, done := trace.Trace(ctx)
	err = d.deployer.Deploy(ctx, p, w)
	done(err, "Deploy",
		"repository", p.Repository.FullName,
		"creator", p.Deployment.Creator.Login,
		"ref", p.Deployment.Ref,
		"sha", p.Deployment.Sha,
	)
	return err
}

// asyncDeployer is a deployer implementation that runs the deployment in a
// goroutine.
type asyncDeployer struct {
	// Should use a tracedDeployer so errors are propagated.
	deployer *tracedDeployer
}

func (d *asyncDeployer) Deploy(ctx context.Context, p events.Deployment, w io.Writer) error {
	go d.deployer.Deploy(ctx, p, w)
	return nil
}
