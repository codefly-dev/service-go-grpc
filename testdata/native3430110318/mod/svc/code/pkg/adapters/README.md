# Hexagonal Architecture

## Adapters

### gRPC

We want to leave the adapters scope as soon as possible to spend as much time as possible in the core package.

Write your RPCs handlers in `rpcs.go`.

### REST

Auto-generated code from protobuf definitions.
