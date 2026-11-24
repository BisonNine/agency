// Package gatewayproto implements the OpenClaw Gateway WebSocket wire protocol:
// frame envelopes, the connect handshake types, device-identity signing, and the
// QR/setup payload. It is stdlib-only (no Agency imports) so it can be shared by
// the gateway server (channels/gateway) and the test node-emulator.
//
// This lets external OpenClaw-compatible devices (e.g. the Rabbit R1) connect to
// Agency. The protocol is reproduced from the OpenClaw reference implementation;
// see the Agency-Rabbit-R1 spec for field-level provenance. Protocol version 4.
package gatewayproto
