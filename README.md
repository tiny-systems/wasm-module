# Tiny Systems WASM Module

Run user-supplied source code as WebAssembly per request. Author writes
program source in node settings, the controller pod compiles it via the
bundled TinyGo toolchain on `OnSettings`, and [wazero](https://wazero.io)
executes the resulting wasm with WASI stdin/stdout JSON as the IO boundary.

Compile errors surface as `TinyNode.Status.Error` exactly like js_eval's
parse errors do — the platform UI renders them red and the MCP
`read_project` exposes them so an agent can self-correct.

## Components

### `wasm_eval`

Author writes a complete program; the component compiles + runs it.

**Settings**

| Field | Type | Required | Notes |
|---|---|---|---|
| `source.language` | enum (`tinygo`) | yes | Compiler to use. TinyGo is bundled. |
| `source.content` | string (code) | yes | Complete program. Reads JSON from stdin, writes JSON to stdout. |
| `inputData` | `configurable: any` | yes | Schema + example of what your program reads from stdin |
| `outputData` | `configurable: any` | yes | Schema + example of what your program writes to stdout |
| `enableErrorPort` | bool | yes | Route runtime failures to the Error port instead of failing |

**Ports**

- `request` — receives `{context, inputData}`. Edge config maps upstream data into `inputData`.
- `response` — emits `{context, outputData}`. Context passes through unchanged; the wasm module does not see it.
- `error` — visible when `enableErrorPort=true`. Emits `{context, error}` for wasm runtime failures.

**Example source (TinyGo)**

```go
package main

import (
    "encoding/json"
    "os"
)

type In struct {
    Name string `json:"name"`
}

type Out struct {
    Greeting string `json:"greeting"`
}

func main() {
    var in In
    _ = json.NewDecoder(os.Stdin).Decode(&in)
    _ = json.NewEncoder(os.Stdout).Encode(Out{
        Greeting: "Hello, " + in.Name,
    })
}
```

Drop that into `Settings.Source.Content`, set `Settings.Source.Language = "tinygo"`,
declare `inputData` as `{name: string}` and `outputData` as `{greeting: string}`,
and the platform handles the rest.

## How the compile loop works

1. `build_flow` (or `edit_flow.configure_node`) writes settings to the TinyNode.
2. The controller delivers `_settings` to the component instance.
3. `OnSettings` writes source to a temp file, runs `tinygo build -target=wasi -no-debug`,
   reads the output bytes, and asks wazero to compile the module.
4. On any failure (compile error, wazero compile error), the SDK records
   `TinyNode.Status.Error`. The UI shows it red; `read_project` returns it.
5. On success, the compiled module is held; each request instantiates it
   fresh and feeds it stdin / collects stdout.

## Why WASM

`js_eval` is great for quick JS transforms. `wasm_eval` covers cases
where you want:

- A typed language with a real type system at compile time
- Native-speed code
- Sandboxed execution by default — the wasm module sees only stdin,
  stdout, stderr, no filesystem, no network, no host
- Compact deterministic artifacts that can be versioned alongside flows

## Bundled toolchain

The operator image is based on `tinygo/tinygo:0.39.0`, so the
controller pod has TinyGo + LLVM-wasi in PATH. The image is ~1.5GB —
larger than a typical operator (distroless ≈ 20MB) — but that's the
cost of inline compilation. Future versions may add Rust or
AssemblyScript targets behind the same Source.Language enum.

## Run locally

```shell
go run cmd/main.go run \
  --name=tinysystems/wasm-module-v0 \
  --namespace=tinysystems-tinysystems \
  --version=0.2.0
```

You'll need TinyGo on your local PATH for compilation to work
outside the container:

```shell
brew install tinygo
```

## Deploy

```shell
helm install wasm-module tinysystems/tinysystems-operator \
  --set controllerManager.manager.image.repository=registry.example.com/wasm-module-v0
```

## Resources

- [Tiny Systems Module SDK](https://github.com/tiny-systems/module)
- [wazero](https://wazero.io) — pure-Go WASM runtime
- [TinyGo](https://tinygo.org) — Go for small places, including wasm32-wasi
- [Tiny Systems Platform](https://tinysystems.io)

## License

MIT-licensed. Depends on the [Tiny Systems Module SDK](https://github.com/tiny-systems/module) (BSL 1.1).
