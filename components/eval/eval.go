// Package eval implements wasm_eval — compile a user-supplied source
// program to wasm32-wasi inside the operator pod, then execute it per
// incoming request using wazero + WASI stdin/stdout as the JSON IO
// boundary. The author writes idiomatic Go (or any language whose
// toolchain we bundle) in Settings.Source.Content; OnSettings compiles
// it via the installed toolchain and stores the resulting module.
//
// Design mirrors js_eval: source lives in Settings, compile errors
// surface from the SettingsHandler back to TinyNode.Status.Error, the
// SDK reflector publishes InputData / OutputData shapes on the
// Request/Response ports so downstream edges validate without
// scenarios.
package eval

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/goccy/go-json"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tiny-systems/module/api/v1alpha1"
	"github.com/tiny-systems/module/module"
	"github.com/tiny-systems/module/registry"
)

const (
	ComponentName = "wasm_eval"
	RequestPort   = "request"
	ResponsePort  = "response"
	ErrorPort     = "error"

	LangTinyGo = "tinygo"
)

// Context, InputData, OutputData are type aliases for `any` to enable
// schema generation via configurable settings — same pattern js_eval
// uses. The concrete shape is supplied by the author in Settings, and
// the SDK reflector publishes it on the relevant ports.
type Context any
type InputData any
type OutputData any

// Source is the user-supplied program. The Language enum drives which
// compiler runs. Content is the program text — for tinygo, a complete
// main package that reads JSON from stdin and writes JSON to stdout.
type Source struct {
	Language string `json:"language" required:"true" default:"tinygo" enum:"tinygo" enumTitles:"TinyGo (Go → wasm32-wasi)" title:"Language" description:"Compiler to use. TinyGo is currently the only supported target."`
	Content  string `json:"content" required:"true" title:"Source code" format:"code" description:"Complete program. For TinyGo, write a main package that reads JSON from os.Stdin and writes JSON to os.Stdout."`
}

type Settings struct {
	EnableErrorPort bool       `json:"enableErrorPort" required:"true" title:"Enable Error Port" description:"If error happens at runtime, error port will emit an error message" tab:"Settings"`
	InputData       InputData  `json:"inputData" configurable:"true" title:"Input shape" description:"Schema and example of expected input (what your program reads from stdin)." tab:"Settings"`
	OutputData      OutputData `json:"outputData" configurable:"true" title:"Output shape" description:"Schema and example of program output (what your program writes to stdout)." tab:"Settings"`
	Source          Source     `json:"source" required:"true" title:"Source" tab:"Source"`
}

type Request struct {
	Context   Context   `json:"context,omitempty" configurable:"true" title:"Context" description:"Arbitrary message passed alongside the input data"`
	InputData InputData `json:"inputData,omitempty" configurable:"true" title:"Input data" description:"JSON passed to the wasm module's stdin"`
}

type Response struct {
	Context    Context    `json:"context"`
	OutputData OutputData `json:"outputData"`
}

type Error struct {
	Context Context `json:"context"`
	Error   string  `json:"error"`
}

// Component holds the wazero runtime + compiled module. The source is
// compiled once on Settings change; per-request work is just module
// instantiation (microseconds with wazero) and WASI execution.
type Component struct {
	settings Settings
	runtime  wazero.Runtime
	compiled wazero.CompiledModule
}

func (c *Component) Instance() module.Component {
	return &Component{}
}

func (c *Component) GetInfo() module.ComponentInfo {
	return module.ComponentInfo{
		Name:        ComponentName,
		Description: "WASM Eval",
		Info: "Compile and run a user-supplied program as WebAssembly per incoming request. " +
			"Write source in Settings.Source.Content (TinyGo for now — read JSON from os.Stdin, " +
			"write JSON to os.Stdout). Declare Settings.inputData (your stdin shape) and " +
			"Settings.outputData (your stdout shape) so edges validate without scenarios. " +
			"Compile errors surface in node status; runtime errors route to the Error port when enabled. " +
			"Context passes through untouched — your program does not see it.",
		Tags: []string{"wasm", "wasi", "tinygo", "eval", "engine"},
	}
}

// OnSettings compiles the user's source into wasm via the bundled
// toolchain, then stores the resulting module for per-request
// execution. Compile failures are returned as errors so the SDK
// reports them through TinyNode.Status, where read_project / the UI
// can read them back.
func (c *Component) OnSettings(ctx context.Context, msg any) error {
	in, ok := msg.(Settings)
	if !ok {
		return fmt.Errorf("invalid settings")
	}
	c.settings = in

	if in.Source.Content == "" {
		return nil
	}

	wasmBytes, err := compileSource(ctx, in.Source.Language, in.Source.Content)
	if err != nil {
		return fmt.Errorf("compile %s: %w", in.Source.Language, err)
	}
	return c.loadWasm(ctx, wasmBytes)
}

// compileSource shells out to the language's toolchain to produce a
// wasm32-wasi binary from source text. Returns the compiled bytes or
// a multi-line error including the toolchain's stderr so authors can
// fix the source.
func compileSource(ctx context.Context, language, content string) ([]byte, error) {
	switch language {
	case LangTinyGo, "":
		return compileTinyGo(ctx, content)
	default:
		return nil, fmt.Errorf("unsupported language %q (only tinygo is bundled)", language)
	}
}

// compileTinyGo writes the source to a temp dir, runs tinygo build
// against the wasi target, and returns the resulting wasm bytes. The
// temp dir is cleaned up before return.
func compileTinyGo(ctx context.Context, content string) ([]byte, error) {
	dir, err := os.MkdirTemp("", "wasm-eval-*")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	srcPath := filepath.Join(dir, "main.go")
	if err := os.WriteFile(srcPath, []byte(content), 0o600); err != nil {
		return nil, fmt.Errorf("write source: %w", err)
	}
	outPath := filepath.Join(dir, "out.wasm")

	cmd := exec.CommandContext(ctx, "tinygo", "build",
		"-target=wasi",
		"-no-debug",
		"-o", outPath,
		srcPath,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("tinygo build failed: %w\n%s", err, stderr.String())
	}

	wasmBytes, err := os.ReadFile(outPath)
	if err != nil {
		return nil, fmt.Errorf("read compiled wasm: %w", err)
	}
	return wasmBytes, nil
}

// loadWasm replaces the runtime + compiled module with a freshly-
// built pair. The prior runtime is closed only after the new one is
// ready so a bad compile doesn't tear down a working component.
func (c *Component) loadWasm(ctx context.Context, wasmBytes []byte) error {
	rt := wazero.NewRuntime(ctx)
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, rt); err != nil {
		_ = rt.Close(ctx)
		return fmt.Errorf("instantiate wasi: %w", err)
	}
	compiled, err := rt.CompileModule(ctx, wasmBytes)
	if err != nil {
		_ = rt.Close(ctx)
		return fmt.Errorf("compile wasm: %w", err)
	}
	if c.runtime != nil {
		_ = c.runtime.Close(context.Background())
	}
	c.runtime = rt
	c.compiled = compiled
	return nil
}

func (c *Component) Handle(ctx context.Context, handler module.Handler, port string, msg any) module.Result {
	if port != RequestPort {
		return module.Fail(fmt.Errorf("unknown port: %s", port))
	}
	in, ok := msg.(Request)
	if !ok {
		return module.Fail(fmt.Errorf("invalid input"))
	}
	if c.compiled == nil {
		return c.handleError(ctx, handler, in.Context, fmt.Errorf("no compiled module — provide Settings.Source.Content"))
	}

	out, err := c.run(ctx, in.InputData)
	if err != nil {
		return c.handleError(ctx, handler, in.Context, err)
	}

	return handler(ctx, ResponsePort, Response{
		Context:    in.Context,
		OutputData: out,
	})
}

// run instantiates the compiled module once per call with stdin
// holding the marshalled InputData and stdout captured into a buffer.
// State is fresh per request so modules can't accidentally leak data
// across invocations.
func (c *Component) run(ctx context.Context, input InputData) (OutputData, error) {
	inputJSON, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("marshal input: %w", err)
	}

	stdin := bytes.NewReader(inputJSON)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}

	cfg := wazero.NewModuleConfig().
		WithStdin(stdin).
		WithStdout(stdout).
		WithStderr(stderr).
		WithArgs("wasm_eval")

	instance, err := c.runtime.InstantiateModule(ctx, c.compiled, cfg)
	if err != nil {
		if stderr.Len() > 0 {
			return nil, fmt.Errorf("wasm exited: %w (stderr: %s)", err, stderr.String())
		}
		return nil, fmt.Errorf("wasm exited: %w", err)
	}
	defer instance.Close(ctx)

	if stdout.Len() == 0 {
		return nil, nil
	}
	var out OutputData
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		return nil, fmt.Errorf("decode output (stdout=%q): %w", stdout.String(), err)
	}
	return out, nil
}

func (c *Component) handleError(ctx context.Context, handler module.Handler, reqCtx Context, err error) module.Result {
	if !c.settings.EnableErrorPort {
		return module.Fail(err)
	}
	return handler(ctx, ErrorPort, Error{
		Context: reqCtx,
		Error:   err.Error(),
	})
}

func (c *Component) Ports() []module.Port {
	ports := []module.Port{
		{
			Name:     RequestPort,
			Label:    "Request",
			Position: module.Left,
			Configuration: Request{
				InputData: c.settings.InputData,
			},
		},
		{
			Name:     ResponsePort,
			Position: module.Right,
			Label:    "Response",
			Source:   true,
			Configuration: Response{
				OutputData: c.settings.OutputData,
			},
		},
		{
			Name:          v1alpha1.SettingsPort,
			Label:         "Settings",
			Configuration: c.settings,
		},
	}
	if !c.settings.EnableErrorPort {
		return ports
	}
	return append(ports, module.Port{
		Position:      module.Bottom,
		Name:          ErrorPort,
		Label:         "Error",
		Source:        true,
		Configuration: Error{},
	})
}

var (
	_ module.Component       = (*Component)(nil)
	_ module.SettingsHandler = (*Component)(nil)
)

func init() {
	registry.Register(&Component{})
}
