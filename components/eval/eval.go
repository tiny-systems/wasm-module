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
//
// Two operational signals on top of that:
//   - phase state guards Handle so requests during compile fail loudly
//     instead of silently returning an empty response or stalling
//   - optional Status output port emits one event per state transition
//     (compiling / ready / error) so flow authors can observe lifecycle
//     in real time without polling
package eval

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

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
	StatusPort    = "status"

	LangTinyGo = "tinygo"

	PhaseUninitialized = ""
	PhaseCompiling     = "compiling"
	PhaseReady         = "ready"
	PhaseError         = "error"
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
	EnableErrorPort  bool       `json:"enableErrorPort" required:"true" title:"Enable Error Port" description:"If runtime errors happen, route them to the Error port instead of failing the request." tab:"Settings"`
	EnableStatusPort bool       `json:"enableStatusPort" required:"true" title:"Enable Status Port" description:"Emit lifecycle events (compiling / ready / error) on a status output port. Useful for observing long-running compiles." tab:"Settings"`
	InputData        InputData  `json:"inputData" configurable:"true" title:"Input shape" description:"Schema and example of expected input (what your program reads from stdin)." tab:"Settings"`
	OutputData       OutputData `json:"outputData" configurable:"true" title:"Output shape" description:"Schema and example of program output (what your program writes to stdout)." tab:"Settings"`
	Source           Source     `json:"source" required:"true" title:"Source" tab:"Source"`
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

// Status is the payload on the optional Status output port. Emitted on
// every phase transition: compiling → ready, or compiling → error. Apps
// can route this port to a debug node, an alerting flow, or wherever
// they want the lifecycle signal.
type Status struct {
	Phase     string `json:"phase" title:"Phase" description:"compiling | ready | error"`
	Message   string `json:"message,omitempty" title:"Message" description:"Free-form detail. For 'error' phase, holds the compile failure text."`
	ElapsedMs int64  `json:"elapsedMs,omitempty" title:"Elapsed (ms)" description:"Wall time since the current phase started"`
}

// Component holds the wazero runtime + compiled module. The source is
// compiled once on Settings change; per-request work is just module
// instantiation (microseconds with wazero) and WASI execution.
//
// phase guards concurrency: Handle reads it under RLock to decide
// whether to run, fail-with-error, or wait. OnSettings writes it
// under Lock when transitioning between compile / ready / error.
type Component struct {
	module.Base

	mu        sync.RWMutex
	settings  Settings
	runtime   wazero.Runtime
	compiled  wazero.CompiledModule
	phase     string
	phaseMsg  string
	phaseTime time.Time
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
			"write JSON to os.Stdout). Declare Settings.inputData and Settings.outputData so " +
			"edges validate without scenarios. Compile errors surface in node status; runtime " +
			"errors route to the Error port when enabled. Enable Settings.EnableStatusPort to " +
			"emit lifecycle events on the Status port — useful because cold compiles can run " +
			"into minutes and you want to know the node isn't dead. Context passes through " +
			"untouched; the wasm module does not see it.",
		Tags: []string{"wasm", "wasi", "tinygo", "eval", "engine"},
	}
}

// OnSettings compiles the user's source into wasm via the bundled
// toolchain, then stores the resulting module for per-request
// execution. Compile failures are returned as errors so the SDK
// reports them through TinyNode.Status. When EnableStatusPort is set,
// the component also emits explicit start / ready / error events on
// the Status port so live observers don't have to poll node status.
func (c *Component) OnSettings(ctx context.Context, msg any) error {
	in, ok := msg.(Settings)
	if !ok {
		return fmt.Errorf("invalid settings")
	}

	c.mu.Lock()
	c.settings = in
	c.mu.Unlock()

	if in.Source.Content == "" {
		c.setPhase(ctx, PhaseUninitialized, "no source provided")
		return nil
	}

	start := time.Now()
	c.setPhase(ctx, PhaseCompiling, "compiling "+in.Source.Language+" source")

	wasmBytes, err := compileSource(ctx, in.Source.Language, in.Source.Content)
	if err != nil {
		c.setPhase(ctx, PhaseError, err.Error())
		return fmt.Errorf("compile %s: %w", in.Source.Language, err)
	}
	if err := c.loadWasm(ctx, wasmBytes); err != nil {
		c.setPhase(ctx, PhaseError, err.Error())
		return err
	}
	c.setPhase(ctx, PhaseReady, fmt.Sprintf("compiled in %s", time.Since(start).Round(time.Millisecond)))
	return nil
}

// setPhase records the new phase and emits a Status event when the
// status port is enabled. Holds the write lock so phase reads from
// Handle stay consistent with the wasm runtime state set alongside it.
func (c *Component) setPhase(ctx context.Context, phase, message string) {
	c.mu.Lock()
	c.phase = phase
	c.phaseMsg = message
	c.phaseTime = time.Now()
	enabled := c.settings.EnableStatusPort
	c.mu.Unlock()

	if !enabled || phase == "" {
		return
	}
	_ = c.Emit(ctx, StatusPort, Status{
		Phase:     phase,
		Message:   message,
		ElapsedMs: 0,
	})
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
	c.mu.Lock()
	prior := c.runtime
	c.runtime = rt
	c.compiled = compiled
	c.mu.Unlock()
	if prior != nil {
		_ = prior.Close(context.Background())
	}
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

	// Snapshot phase + compiled module under read lock so we don't race
	// with OnSettings swapping the runtime mid-handle.
	c.mu.RLock()
	phase := c.phase
	phaseMsg := c.phaseMsg
	phaseTime := c.phaseTime
	c.mu.RUnlock()

	// Refuse the request loudly when there's nothing usable to run.
	// This guards against "silently returns empty" while a long compile
	// is in flight — callers see the actual state instead of waiting
	// on an unresponsive handler.
	switch phase {
	case PhaseUninitialized:
		return c.handleError(ctx, handler, in.Context, fmt.Errorf("no compiled module — provide Settings.Source.Content"))
	case PhaseCompiling:
		elapsed := time.Since(phaseTime).Round(time.Second)
		return c.handleError(ctx, handler, in.Context, fmt.Errorf("still compiling (%s elapsed): %s", elapsed, phaseMsg))
	case PhaseError:
		return c.handleError(ctx, handler, in.Context, fmt.Errorf("last compile failed: %s", phaseMsg))
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
	c.mu.RLock()
	compiled := c.compiled
	runtime := c.runtime
	c.mu.RUnlock()
	if compiled == nil || runtime == nil {
		return nil, fmt.Errorf("no compiled module")
	}

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

	instance, err := runtime.InstantiateModule(ctx, compiled, cfg)
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
	if c.settings.EnableErrorPort {
		ports = append(ports, module.Port{
			Position:      module.Bottom,
			Name:          ErrorPort,
			Label:         "Error",
			Source:        true,
			Configuration: Error{},
		})
	}
	if c.settings.EnableStatusPort {
		ports = append(ports, module.Port{
			Position:      module.Bottom,
			Name:          StatusPort,
			Label:         "Status",
			Source:        true,
			Configuration: Status{},
		})
	}
	return ports
}

var (
	_ module.Component       = (*Component)(nil)
	_ module.SettingsHandler = (*Component)(nil)
)

func init() {
	registry.Register(&Component{})
}
