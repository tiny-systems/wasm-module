# Custom final stage — overrides the SDK's default distroless image so
# the running operator pod has the TinyGo toolchain available. The
# wasm_eval component shells out to `tinygo build -target=wasi` during
# OnSettings to compile user-supplied source into wasm.
#
# The SDK's build tool prepends its Dockerfile-base which produces
# /bin/manager from the Go source; this final stage copies that binary
# in and inherits CA certs.
FROM tinygo/tinygo:0.39.0
COPY --from=builder /bin/manager /manager
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
# tinygo image runs as root; the operator manager only spawns
# short-lived `tinygo build` subprocesses against /tmp scratch dirs.
CMD ["/manager", "run"]
