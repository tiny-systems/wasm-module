# Tiny Systems WASM Module

Run user-supplied WebAssembly modules as flow components. The WASM runtime
is [wazero](https://wazero.io) — pure Go, no CGO. Authors compile any
language to `wasm32-wasi` (TinyGo, Rust, AssemblyScript, Zig) and drop
the resulting `.wasm` binary into a `wasm_eval` node's settings.

## Components

### `wasm_eval`

Executes a WASI command module per incoming request, using stdin/stdout
as the JSON IO boundary. Mirrors the shape of `js_eval` from
`js-module-v0` — author declares the input shape, output shape, and the
module binary in settings; downstream edges get validated against the
declared output shape with no scenarios required.

**Settings**

| Field | Type | Required | Notes |
|---|---|---|---|
| `module` | `{name, content}` | yes | Compiled `wasm32-wasi` binary |
| `inputData` | `configurable: any` | yes | Schema + example of what the wasm reads from stdin |
| `outputData` | `configurable: any` | yes | Schema + example of what the wasm writes to stdout |
| `enableErrorPort` | `bool` | yes | Route runtime failures to the Error port instead of failing |

**Ports**

- `request` — receives `{context, inputData}`. Edge config maps upstream data into `inputData`.
- `response` — emits `{context, outputData}`. Context is passed through unchanged; the wasm module does not see it.
- `error` — visible when `enableErrorPort=true`. Emits `{context, error}` for wasm failures.

**ABI**

The wasm module is invoked as a WASI command (the binary's `_start`
runs to completion per request). Communication is JSON over stdin /
stdout:

```rust
// Rust + wasm32-wasi
use std::io::{Read, Write};
use serde_json::Value;

fn main() {
    let mut input = String::new();
    std::io::stdin().read_to_string(&mut input).unwrap();
    let req: Value = serde_json::from_str(&input).unwrap();

    let resp = serde_json::json!({
        "greeting": format!("Hello, {}", req["name"]),
    });
    print!("{}", resp);
}
```

```go
// TinyGo + wasi
package main

import (
    "encoding/json"
    "os"
)

type Req struct{ Name string `json:"name"` }
type Resp struct{ Greeting string `json:"greeting"` }

func main() {
    var r Req
    _ = json.NewDecoder(os.Stdin).Decode(&r)
    _ = json.NewEncoder(os.Stdout).Encode(Resp{Greeting: "Hello, " + r.Name})
}
```

Build with:
- Rust: `cargo build --target wasm32-wasi --release` → `target/wasm32-wasi/release/<crate>.wasm`
- TinyGo: `tinygo build -o module.wasm -target=wasi main.go`

## Why WASM

`js_eval` is great for quick JS transforms. `wasm_eval` covers the rest:

- Any compiled language (Rust, Go via TinyGo, C/C++, Zig, AssemblyScript)
- Native-speed code
- Sandboxed by default — the wasm module sees only stdin/stdout/stderr, no host access
- Compact, deterministic artifacts that can be versioned alongside flows

## Run locally

```shell
go run cmd/main.go run \
  --name=tinysystems/wasm-module-v0 \
  --namespace=tinysystems-tinysystems \
  --version=0.1.0
```

## Deploy

```shell
# Build container image
docker build -t myregistry/wasm-module-v0:0.1.0 .
docker push myregistry/wasm-module-v0:0.1.0

# Install via Helm
helm install wasm-module tinysystems/tinysystems-operator \
  --set controllerManager.manager.image.repository=myregistry/wasm-module-v0
```

## Resources

- [Tiny Systems Module SDK](https://github.com/tiny-systems/module) — core library
- [wazero](https://wazero.io) — pure-Go WASM runtime used here
- [WASI spec](https://wasi.dev) — the system interface authors target
- [Tiny Systems Platform](https://tinysystems.io) — visual editor and module directory

## License

MIT-licensed. Depends on the [Tiny Systems Module SDK](https://github.com/tiny-systems/module) (BSL 1.1). See [LICENSE](LICENSE) for details.
