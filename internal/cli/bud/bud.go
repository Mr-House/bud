package bud

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/livebud/bud/internal/gobuild"

	"github.com/livebud/bud/internal/extrafile"

	"github.com/livebud/bud/internal/current"
	"github.com/livebud/bud/internal/errs"
	"github.com/livebud/bud/internal/exe"
	"github.com/livebud/bud/internal/pubsub"
	"golang.org/x/mod/semver"

	"github.com/livebud/bud/framework"
	"github.com/livebud/bud/framework/app"
	"github.com/livebud/bud/framework/controller"
	"github.com/livebud/bud/framework/generate"
	"github.com/livebud/bud/framework/generator"
	"github.com/livebud/bud/framework/public"
	"github.com/livebud/bud/framework/transform/transformrt"
	"github.com/livebud/bud/framework/view"
	"github.com/livebud/bud/framework/view/dom"
	"github.com/livebud/bud/framework/view/ssr"
	"github.com/livebud/bud/framework/web"
	"github.com/livebud/bud/package/commander"
	"github.com/livebud/bud/package/di"
	"github.com/livebud/bud/package/gomod"
	"github.com/livebud/bud/package/js"
	v8 "github.com/livebud/bud/package/js/v8"
	"github.com/livebud/bud/package/log"
	"github.com/livebud/bud/package/log/console"
	"github.com/livebud/bud/package/log/filter"
	"github.com/livebud/bud/package/overlay"
	"github.com/livebud/bud/package/parser"
	"github.com/livebud/bud/package/remotefs"
	"github.com/livebud/bud/package/socket"
	"github.com/livebud/bud/package/svelte"
	"github.com/livebud/bud/package/vfs"
)

// Input contains the configuration that gets passed into the commands
type Input struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	Env    []string

	// Currently passed in only for testing
	Dir   string          // Can be empty
	BudLn socket.Listener // Can be nil
	WebLn socket.Listener // Can be nil
	Bus   pubsub.Client   // Can be nil
}

func New(in *Input) *Command {
	return &Command{in: in}
}

type Command struct {
	in   *Input
	Dir  string
	Log  string
	Args []string
	Help bool
}

// Run a custom command
// TODO: finish supporting custom commands
// 1. Compile
//   a. Generate generator (later!)
//   	 i. Generate bud/internal/generator
//     ii. Build bud/generator
//     iii. Run bud/generator
//   b. Generate custom command
//     i. Generate bud/internal/command/${name}/
//     ii. Build bud/command/${name}
// 2. Run bud/command/${name}
func (c *Command) Run(ctx context.Context) error {
	return commander.Usage()
}

const minGoVersion = "v1.17"

// ErrMinGoVersion error is returned when Bud needs a newer version of Go
var ErrMinGoVersion = fmt.Errorf("bud requires Go %s or later", minGoVersion)

// CheckGoVersion checks if the current version of Go is greater than the
// minimum required Go version.
func CheckGoVersion(currentVersion string) error {
	currentVersion = "v" + strings.TrimPrefix(currentVersion, "go")
	// If we encounter an invalid version, it's probably a development version of
	// Go. We'll let those pass through. Reference:
	// https://github.com/golang/go/blob/3cf79d96105d890d7097d274804644b2a2093df1/src/runtime/extern.go#L273-L275
	if !semver.IsValid(currentVersion) {
		return nil
	}
	if semver.Compare(currentVersion, minGoVersion) < 0 {
		return ErrMinGoVersion
	}
	return nil
}

// Module finds the go.mod file for the application
func Module(dir string) (*gomod.Module, error) {
	return gomod.Find(dir)
}

// BudModule finds the module not of your app, but of bud itself
func BudModule() (*gomod.Module, error) {
	dirname, err := current.Directory()
	if err != nil {
		return nil, err
	}
	return gomod.Find(dirname)
}

func Log(stderr io.Writer, logFilter string) (log.Interface, error) {
	handler, err := filter.Load(console.New(stderr), logFilter)
	if err != nil {
		return nil, err
	}
	return log.New(handler), nil
}

func FileSystem(ctx context.Context, log log.Interface, module *gomod.Module, flag *framework.Flag, in *Input) (*overlay.FileSystem, func() error, error) {
	closers := []func() error{}
	closer := func() (err error) {
		for i := len(closers) - 1; i >= 0; i-- {
			err = errs.Join(err, closers[i]())
		}
		return err
	}
	genfs, err := overlay.Load(log, module)
	if err != nil {
		return nil, closer, err
	}
	parser := parser.New(genfs, module)
	injector := di.New(genfs, log, module, parser)
	vm, err := v8.Load()
	if err != nil {
		return nil, closer, err
	}
	svelteCompiler, err := svelte.Load(vm)
	if err != nil {
		return nil, closer, err
	}
	transforms, err := transformrt.Load(svelte.NewTransformable(svelteCompiler))
	if err != nil {
		return nil, closer, err
	}
	genfs.FileGenerator("bud/internal/app/main.go", app.New(injector, module, flag))
	genfs.FileGenerator("bud/internal/app/web/web.go", web.New(module, parser))
	genfs.FileGenerator("bud/internal/app/controller/controller.go", controller.New(injector, module, parser))
	genfs.FileGenerator("bud/internal/app/view/view.go", view.New(module, transforms, flag))
	genfs.FileGenerator("bud/internal/app/public/public.go", public.New(flag))
	genfs.FileGenerator("bud/internal/generate/main.go", generate.New(injector, module))
	genfs.FileGenerator("bud/internal/generate/generator/generator.go", generator.New(module, parser))
	// Sync generate now to support custom generators, if any
	if err := genfs.Sync("bud/internal/generate"); err != nil {
		return nil, closer, err
	}
	// Support custom generators
	if err := vfs.Exist(module, "bud/internal/generate/main.go"); nil == err {
		// Build the app
		builder := gobuild.New(module)
		if err := builder.Build(ctx, "bud/internal/generate/main.go", "bud/generate"); err != nil {
			return nil, closer, err
		}
		// TODO: should we use a unix domain socket instead?
		ln, err := socket.Listen(":0")
		if err != nil {
			return nil, closer, err
		}
		closers = append(closers, ln.Close)
		netFile, err := ln.File()
		if err != nil {
			return nil, closer, err
		}
		closers = append(closers, netFile.Close)
		cmd := &exe.Command{
			Stdin:  in.Stdin,
			Stdout: in.Stdout,
			Stderr: in.Stderr,
			Dir:    module.Directory(),
			Env:    in.Env,
		}
		extrafile.Inject(&cmd.ExtraFiles, &cmd.Env, "GENERATE", netFile)
		// Start the app
		process, err := cmd.Start(ctx, "./bud/generate")
		if err != nil {
			return nil, closer, err
		}
		closers = append(closers, process.Close)
		remotefs, err := remotefs.Dial(ctx, ln.Addr().String())
		if err != nil {
			return nil, closer, err
		}
		closers = append(closers, remotefs.Close)
		genfs.Mount("bud/internal/generator", remotefs)
	}
	return genfs, closer, nil
}

func FileServer(log log.Interface, module *gomod.Module, vm js.VM, flag *framework.Flag) (*overlay.Server, error) {
	servefs, err := overlay.Serve(log, module)
	if err != nil {
		return nil, err
	}
	svelteCompiler, err := svelte.Load(vm)
	if err != nil {
		return nil, err
	}
	transforms, err := transformrt.Load(svelte.NewTransformable(svelteCompiler))
	if err != nil {
		return nil, err
	}
	servefs.FileGenerator("bud/view/_ssr.js", ssr.New(module, transforms.SSR))
	servefs.FileServer("bud/view", dom.New(module, transforms.DOM))
	servefs.FileServer("bud/node_modules", dom.NodeModules(module))
	return servefs, nil
}

// EnsureVersionAlignment ensures that the CLI and runtime versions are aligned.
// If they're not aligned, the CLI will correct the go.mod file to align them.
func EnsureVersionAlignment(ctx context.Context, module *gomod.Module, budVersion string) error {
	modfile := module.File()
	// Do nothing for the latest version
	if budVersion == "latest" {
		// If the module file already replaces bud, don't do anything.
		if modfile.Replace(`github.com/livebud/bud`) != nil {
			return nil
		}
		// Best effort attempt to replace bud with the latest version.
		budModule, err := BudModule()
		if err != nil {
			return nil
		}
		// Replace bud with the local version if we found it.
		if err := modfile.AddReplace("github.com/livebud/bud", "", budModule.Directory(), ""); err != nil {
			return err
		}
		// Write the go.mod file back to disk.
		if err := os.WriteFile(module.Directory("go.mod"), modfile.Format(), 0644); err != nil {
			return err
		}
		return nil
	}
	target := "v" + budVersion
	require := modfile.Require("github.com/livebud/bud")
	// We're good, the CLI matches the runtime version
	if require != nil && require.Version == target {
		return nil
	}
	// Otherwise, update go.mod to match the CLI's version
	if err := modfile.AddRequire("github.com/livebud/bud", target); err != nil {
		return err
	}
	if err := os.WriteFile(module.Directory("go.mod"), modfile.Format(), 0644); err != nil {
		return err
	}
	// Run `go mod download`
	cmd := exec.CommandContext(ctx, "go", "mod", "download")
	cmd.Dir = module.Directory()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	if err := cmd.Run(); err != nil {
		return err
	}
	return nil
}
