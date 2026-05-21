// Package protocol defines the wire format spoken between xerottyd
// (the headless daemon) and xerotty (the UI client). Frames are
// length-prefixed msgpack — a u32 big-endian length followed by a
// msgpack-encoded message body. Each message struct in this package
// has a //go:generate msgp directive; build.sh runs codegen before
// every build so the Encode/Decode methods are always in sync with
// the struct definitions.
//
// Why msgpack: structured binary, ~1x JSON size, ~5-10x faster
// encode/decode, native libs in every language so third-party clients
// (phone, web, Xyphia bindings) are easy. Why tinylib/msgp's codegen
// over vmihailenco's reflection-based library: ~5-10x faster on hot
// paths, zero allocations per encode/decode. The build pipeline hides
// the codegen step.
//
// Message types are versioned via a Hello handshake (see Hello /
// HelloAck). Major-version bumps are a breaking change and require
// recompiling both sides; minor-version bumps add fields msgpack can
// gracefully skip on older readers.
//
// See docs/DAEMON_PLAN.md for the architecture this protocol serves.
package protocol
