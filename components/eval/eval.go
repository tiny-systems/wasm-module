// Package eval implements wasm_eval — execute a user-supplied WebAssembly
// module per incoming request, using WASI stdin/stdout as the JSON IO
// boundary. Authors compile any language to wasm32-wasi (TinyGo, Rust,
// AssemblyScript, Zig, etc.), shape the module's main to read JSON from
// stdin and write JSON to stdout, and the platform handles the rest.
//
// The design mirrors js_eval: input and output shapes live in Settings
// (declared as configurable fields so the SDK reflector publishes them
// for edge validation), the user binary lives in Settings.Module as the
// equivalent of js_eval's Script. The Request port carries inputData
// configured by the incoming edge; the Response port emits outputData.
package eval

import (
	"bytes"
	"context"
	"fmt"

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
)

// Context, InputData, OutputData are type aliases for `any` to enable
// schema generation via configurable settings — same pattern js_eval
// uses. The concrete shape is supplied by the author in Settings, and
// the SDK reflector publishes it on the relevant ports.
type Context any
type InputData any
type OutputData any

// Module is the user-supplied WebAssembly artifact. Authors upload a
// compiled .wasm binary as Content. Name is metadata for display.
type Module struct {
	Name    string `json:"name" required:"true" title:"File name" description:"e.g. transform.wasm"`
	Content []byte `json:"content" required:"true" title:"WASM binary" description:"Compiled WebAssembly module (wasm32-wasi target). Read JSON from stdin, write JSON to stdout." format:"binary"`
}

type Settings struct {
	EnableErrorPort bool       `json:"enableErrorPort" required:"true" title:"Enable Error Port" description:"If error happens, error port will emit an error message" tab:"Settings"`
	InputData       InputData  `json:"inputData" configurable:"true" title:"Input shape" description:"Schema and example of expected input (what your wasm reads from stdin)." tab:"Settings"`
	OutputData      OutputData `json:"outputData" configurable:"true" title:"Output shape" description:"Schema and example of script output (what your wasm writes to stdout)." tab:"Settings"`
	Module          Module     `json:"module" required:"true" title:"WASM module" description:"Compiled wasm32-wasi binary. The module's entry point reads JSON from stdin and writes JSON to stdout." tab:"Module"`
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

// Component holds the wazero runtime + compiled module. Compiled once
// on settings change and reused across requests — wazero is designed
// for this and pays the parse/validate cost only once.
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
		Info: "Run a user-supplied WebAssembly module per incoming request. The wasm module reads JSON " +
			"input from stdin and writes JSON output to stdout — define Settings.inputData (what your " +
			"module expects) and Settings.outputData (what it returns) so edges can validate without " +
			"running the module. Context is NOT passed into the module; it passes through automatically " +
			"from request to response. Compile any language to wasm32-wasi (TinyGo, Rust, AssemblyScript, " +
			"Zig) — the wasm runtime is wazero, pure Go, no CGO.",
		Tags: []string{"wasm", "wasi", "eval", "engine"},
	}
}

// OnSettings recompiles the wasm module whenever settings change. We
// keep the prior compiled module valid until the new compile succeeds
// so a bad upload doesn't break in-flight requests.
func (c *Component) OnSettings(ctx context.Context, msg any) error {
	in, ok := msg.(Settings)
	if !ok {
		return fmt.Errorf("invalid settings")
	}
	c.settings = in
	if len(in.Module.Content) == 0 {
		return nil
	}
	return c.compile(ctx, in.Module.Content)
}

// compile (re)builds the wazero runtime + compiled module from raw
// bytes. Replaces any prior runtime — wazero modules are tied to a
// runtime so we can't hot-swap individual modules.
func (c *Component) compile(ctx context.Context, wasmBytes []byte) error {
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
	// Close any prior runtime now that the new one is ready.
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
		return c.handleError(ctx, handler, in.Context, fmt.Errorf("no wasm module loaded — set Settings.Module"))
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
// The module's _start (WASI command convention) runs to completion;
// stdout is then unmarshalled as OutputData.
//
// Per-call instantiation keeps state isolated between requests — a
// module can't accidentally leak data across invocations. wazero's
// CompiledModule reuse means instantiation is cheap (microseconds).
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
		WithArgs(c.settings.Module.Name)

	instance, err := c.runtime.InstantiateModule(ctx, c.compiled, cfg)
	if err != nil {
		// WASI command modules return an exit code via a special
		// wazero error. A non-zero exit is a script-side failure;
		// surface what the module wrote to stderr to help the author.
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
