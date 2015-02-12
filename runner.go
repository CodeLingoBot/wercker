package main

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"os/signal"
	"path/filepath"

	"code.google.com/p/go-uuid/uuid"
	"github.com/chuckpreslar/emission"
	"github.com/termie/go-shutil"
	"golang.org/x/net/context"
)

// GetPipeline is a function that will fetch the appropriate pipeline
// object from the rawConfig.
type GetPipeline func(*RawConfig, *PipelineOptions) (Pipeline, error)

// GetBuildPipeline grabs the "build" section of the yaml.
func GetBuildPipeline(rawConfig *RawConfig, options *PipelineOptions) (Pipeline, error) {
	build, err := rawConfig.RawBuild.ToBuild(options)
	if err != nil {
		return nil, err
	}
	return build, nil
}

// GetDeployPipeline gets the "deploy" section of the yaml.
func GetDeployPipeline(rawConfig *RawConfig, options *PipelineOptions) (Pipeline, error) {
	build, err := rawConfig.RawDeploy.ToDeploy(options)
	if err != nil {
		return nil, err
	}
	return build, nil
}

// Runner is the base type for running the pipelines.
type Runner struct {
	options        *PipelineOptions
	emitter        *emission.Emitter
	literalLogger  *LiteralLogHandler
	metrics        *MetricsEventHandler
	reporter       *ReportHandler
	pipelineGetter GetPipeline
	murder         *LogEntry
}

// NewRunner from global options
func NewRunner(options *PipelineOptions, pipelineGetter GetPipeline) *Runner {

	e := GetEmitter()
	murder := rootLogger.WithField("Logger", "Runner")
	// h, err := NewLogHandler()
	// if err != nil {
	//   p.murder.WithField("Error", err).Panic("Unable to LogHandler")
	// }
	// h.ListenTo(e)

	if options.Debug {
		dh := NewDebugHandler()
		dh.ListenTo(e)
	}

	l, err := NewLiteralLogHandler(options)
	if err != nil {
		murder.WithField("Error", err).Panic("Unable to LiteralLogHandler")
	}
	l.ListenTo(e)

	var mh *MetricsEventHandler
	if options.ShouldKeenMetrics {
		mh, err = NewMetricsHandler(options)
		if err != nil {
			murder.WithField("Error", err).Panic("Unable to MetricsHandler")
		}
		mh.ListenTo(e)
	}

	var r *ReportHandler
	if options.ShouldReport {
		r, err := NewReportHandler(options.ReporterHost, options.ReporterKey)
		if err != nil {
			murder.WithField("Error", err).Panic("Unable to ReportHandler")
		}
		r.ListenTo(e)
	}

	return &Runner{
		options:        options,
		emitter:        e,
		literalLogger:  l,
		metrics:        mh,
		reporter:       r,
		pipelineGetter: pipelineGetter,
		murder:         murder,
	}
}

// Emitter shares the Runner's emitter.
func (p *Runner) Emitter() *emission.Emitter {
	return p.emitter
}

// ProjectDir returns the directory where we expect to find the code for this project
func (p *Runner) ProjectDir() string {
	return fmt.Sprintf("%s/%s", p.options.ProjectDir, p.options.ApplicationID)
}

// EnsureCode makes sure the code is in the ProjectDir.
// NOTE(termie): When launched by kiddie-pool the ProjectPath will be
// set to the location where grappler checked out the code and the copy
// will be a little superfluous, but in the case where this is being
// run in Single Player Mode this copy is necessary to avoid screwing
// with the local dir.
func (p *Runner) EnsureCode() (string, error) {
	projectDir := p.ProjectDir()

	// If the target is a tarball feetch and build that
	if p.options.ProjectURL != "" {
		resp, err := fetchTarball(p.options.ProjectURL)
		if err != nil {
			return projectDir, err
		}
		err = untargzip(projectDir, resp.Body)
		if err != nil {
			return projectDir, err
		}
	} else {
		// We were pointed at a path with ProjectPath, copy it to projectDir

		ignoreFiles := []string{
			p.options.BuildDir,
			p.options.ProjectDir,
			p.options.StepDir,
			p.options.ContainerDir,
		}

		// Make sure we don't accidentally recurse or copy extra files
		ignoreFunc := func(src string, files []os.FileInfo) []string {
			ignores := []string{}
			for _, file := range files {
				abspath, err := filepath.Abs(filepath.Join(src, file.Name()))
				if err != nil {
					// Something went sufficiently wrong
					panic(err)
				}

				if ContainsString(ignoreFiles, abspath) {
					ignores = append(ignores, file.Name())
				}
				// TODO(termie): maybe ignore .gitignore files?
			}
			return ignores
		}
		copyOpts := &shutil.CopyTreeOptions{Ignore: ignoreFunc, CopyFunction: shutil.Copy}
		os.Rename(projectDir, fmt.Sprintf("%s-%s", projectDir, uuid.NewRandom().String()))
		err := shutil.CopyTree(p.options.ProjectPath, projectDir, copyOpts)
		if err != nil {
			return projectDir, err
		}
	}
	return projectDir, nil
}

// GetConfig parses and returns the wercker.yml file.
func (p *Runner) GetConfig() (*RawConfig, string, error) {
	// Return a []byte of the yaml we find or create.
	var werckerYaml []byte
	var err error
	if p.options.WerckerYml != "" {
		werckerYaml, err = ioutil.ReadFile(p.options.WerckerYml)
		if err != nil {
			return nil, "", err
		}
	} else {
		werckerYaml, err = ReadWerckerYaml([]string{p.ProjectDir()}, false)
		if err != nil {
			return nil, "", err
		}
	}

	// Parse that bad boy.
	rawConfig, err := ConfigFromYaml(werckerYaml)
	if err != nil {
		return nil, "", err
	}

	// Add some options to the global config
	if rawConfig.SourceDir != "" {
		p.options.SourceDir = rawConfig.SourceDir
	}

	return rawConfig, string(werckerYaml), nil
}

// GetBox fetches and returns the base box for the pipeline.
func (p *Runner) GetBox(rawConfig *RawConfig) (*Box, error) {
	// Promote RawBox to a real Box. We believe in you, Box!
	box, err := rawConfig.RawBox.ToBox(p.options, nil)
	if err != nil {
		return nil, err
	}

	p.murder.Println("Box:", box.Name)

	// Make sure we have the box available
	image, err := box.Fetch()
	if err != nil {
		return nil, err
	}
	p.murder.Println("Docker Image:", image.ID)
	return box, nil
}

// AddServices fetches and links the services to the base box.
func (p *Runner) AddServices(rawConfig *RawConfig, box *Box) error {
	for _, rawService := range rawConfig.RawServices {
		p.murder.Println("Fetching service:", rawService)

		serviceBox, err := rawService.ToServiceBox(p.options, nil)
		if err != nil {
			return err
		}

		if _, err := serviceBox.Box.Fetch(); err != nil {
			return err
		}

		box.AddService(serviceBox)
		// TODO(mh): We want to make sure container is running fully before
		// allowing build steps to run. We may need custom steps which block
		// until service services are running.
	}
	return nil
}

// CopySource copies the source into the HostPath
func (p *Runner) CopySource() error {
	// Start setting up the pipeline dir
	err := os.MkdirAll(p.options.HostPath(), 0755)
	if err != nil {
		return err
	}

	err = shutil.CopyTree(p.ProjectDir(), p.options.HostPath("source"), nil)
	if err != nil {
		return err
	}
	return nil
}

// GetSession attaches to the container and returns a session.
func (p *Runner) GetSession(runnerContext context.Context, containerID string) (context.Context, *Session, error) {
	dockerTransport, err := NewDockerTransport(p.options, containerID)
	if err != nil {
		return nil, nil, err
	}
	sess := NewSession(p.options, dockerTransport)
	if err != nil {
		return nil, nil, err
	}
	sessionCtx, err := sess.Attach(runnerContext)
	if err != nil {
		return nil, nil, err
	}

	return sessionCtx, sess, nil
}

// GetPipeline returns a pipeline based on the "build" config section
func (p *Runner) GetPipeline(rawConfig *RawConfig) (Pipeline, error) {
	return p.pipelineGetter(rawConfig, p.options)
}

// RunnerShared holds on to the information we got from setting up our
// environment.
type RunnerShared struct {
	box         *Box
	pipeline    Pipeline
	sess        *Session
	config      *RawConfig
	sessionCtx  context.Context
	containerID string
}

// StartStep emits BuildStepStarted and returns a Finisher for the end event.
func (p *Runner) StartStep(ctx *RunnerShared, step *Step, order int) *Finisher {
	p.emitter.Emit(BuildStepStarted, &BuildStepStartedArgs{
		Options: p.options,
		Box:     ctx.box,
		Build:   ctx.pipeline,
		Step:    step,
		Order:   order,
	})
	return NewFinisher(func(result interface{}) {
		r := result.(*StepResult)
		artifactURL := ""
		if r.Artifact != nil {
			artifactURL = r.Artifact.URL()
		}
		p.emitter.Emit(BuildStepFinished, &BuildStepFinishedArgs{
			Options:             p.options,
			Box:                 ctx.box,
			Build:               ctx.pipeline,
			Step:                step,
			Order:               order,
			Successful:          r.Success,
			Message:             r.Message,
			ArtifactURL:         artifactURL,
			PackageURL:          r.PackageURL,
			WerckerYamlContents: r.WerckerYamlContents,
		})
	})
}

// StartBuild emits a BuildStarted and returns for a Finisher for the end.
func (p *Runner) StartBuild(options *PipelineOptions) *Finisher {
	p.emitter.Emit(BuildStarted, &BuildStartedArgs{Options: options})
	return NewFinisher(func(result interface{}) {
		r, ok := result.(*BuildFinishedArgs)
		if !ok {
			return
		}
		r.Options = options
		p.emitter.Emit(BuildFinished, r)
	})
}

// StartFullPipeline emits a FullPipelineFinished when the Finisher is called.
func (p *Runner) StartFullPipeline(options *PipelineOptions) *Finisher {
	return NewFinisher(func(result interface{}) {
		r, ok := result.(*FullPipelineFinishedArgs)
		if !ok {
			return
		}

		r.Options = options
		p.emitter.Emit(FullPipelineFinished, r)
	})
}

// SetupEnvironment does a lot of boilerplate legwork and returns a pipeline,
// box, and session. This is a bit of a long method, but it is pretty much
// the entire "Setup Environment" step.
func (p *Runner) SetupEnvironment(runnerCtx context.Context) (*RunnerShared, error) {
	shared := &RunnerShared{}

	sr := &StepResult{
		Success:  false,
		Artifact: nil,
		Message:  "",
		ExitCode: 1,
	}

	setupEnvironmentStep := &Step{Owner: "wercker", Name: "setup environment", Version: Version()}
	finisher := p.StartStep(shared, setupEnvironmentStep, 2)
	defer finisher.Finish(sr)

	p.murder.Println("Application:", p.options.ApplicationName)

	// Grab our config
	rawConfig, stringConfig, err := p.GetConfig()
	if err != nil {
		return shared, err
	}
	shared.config = rawConfig
	sr.WerckerYamlContents = stringConfig

	box, err := p.GetBox(rawConfig)
	if err != nil {
		return shared, err
	}
	shared.box = box

	err = p.AddServices(rawConfig, box)
	if err != nil {
		return shared, err
	}

	// Start setting up the pipeline dir
	p.murder.Debugln("Copying source to build directory")
	err = p.CopySource()
	if err != nil {
		return shared, err
	}

	pipeline, err := p.GetPipeline(rawConfig)
	shared.pipeline = pipeline

	p.murder.Println("Steps:", len(pipeline.Steps()))

	// Make sure we have the steps
	err = pipeline.FetchSteps()
	if err != nil {
		return shared, err
	}

	// Start booting up our services
	// TODO(termie): maybe move this into box.Run?
	err = box.RunServices()
	if err != nil {
		return shared, err
	}

	// Boot up our main container
	container, err := box.Run()
	if err != nil {
		return shared, err
	}
	shared.containerID = container.ID

	// Register our signal handler to clean the box up
	// TODO(termie): we should probably make a little general purpose signal
	// handler and register callbacks with it so that multiple parts of the app
	// can do cleanup
	sigint := make(chan os.Signal, 1)
	signal.Notify(sigint, os.Interrupt)
	go func() {
		tries := 0
		for _ = range sigint {
			if tries == 0 {
				tries = 1
				box.Stop()
				os.Exit(1)
			} else {
				panic("Exiting forcefully")
			}
		}
	}()

	p.murder.Debugln("Attaching session to base box")
	// Start our session
	sessionCtx, sess, err := p.GetSession(runnerCtx, container.ID)
	if err != nil {
		return shared, err
	}
	shared.sess = sess
	shared.sessionCtx = sessionCtx

	// Some helpful logging
	pipeline.LogEnvironment()

	p.murder.Debugln("Setting up guest (base box)")
	err = pipeline.SetupGuest(sessionCtx, sess)
	if err != nil {
		return shared, err
	}

	err = pipeline.ExportEnvironment(sessionCtx, sess)
	if err != nil {
		return shared, err
	}

	sr.Success = true
	sr.ExitCode = 0
	return shared, nil
}

// StepResult holds the info we need to report on steps
type StepResult struct {
	Success             bool
	Artifact            *Artifact
	PackageURL          string
	Message             string
	ExitCode            int
	WerckerYamlContents string
}

// RunStep runs a step and tosses error if it fails
func (p *Runner) RunStep(shared *RunnerShared, step *Step, order int) (*StepResult, error) {
	finisher := p.StartStep(shared, step, order)
	sr := &StepResult{
		Success:  false,
		Artifact: nil,
		Message:  "",
		ExitCode: 1,
	}
	defer finisher.Finish(sr)

	step.InitEnv()
	p.murder.Println("Step Environment")
	for _, pair := range step.Env.Ordered() {
		p.murder.Println(" ", pair[0], pair[1])
	}

	exit, err := step.Execute(shared.sessionCtx, shared.sess)
	if exit != 0 {
		sr.ExitCode = exit
	} else if err != nil {
		return sr, err
	} else {
		sr.Success = true
		sr.ExitCode = 0
	}

	// Grab the message
	var message bytes.Buffer
	err = step.CollectFile(shared.containerID, step.ReportPath(), "message.txt", &message)
	if err != nil {
		if err != ErrEmptyTarball {
			return sr, err
		}
	}
	sr.Message = message.String()

	// Grab artifacts if we want them
	if p.options.ShouldArtifacts {
		artifact, err := step.CollectArtifact(shared.containerID)
		if err != nil {
			return sr, err
		}

		if artifact != nil {
			artificer := NewArtificer(p.options)
			err = artificer.Upload(artifact)
			if err != nil {
				return sr, err
			}
		}
		sr.Artifact = artifact
	}

	if !sr.Success {
		return sr, fmt.Errorf("Step failed with exit code: %d", sr.ExitCode)
	}
	return sr, nil
}
