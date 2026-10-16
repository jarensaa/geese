# Transports

geese knows no transport. Everything a transport needs is in the core package:

| | |
| --- | --- |
| `flock.Admit(ctx)` | Admits a unit of work: a request, a call, a stream, a message from a queue. A goose that loses its place cancels it. |
| `flock.Ready()` | The health check: false while the goose is grounded. |
| `flock.Capabilities(ctx)` | What clients read: what's switched on, and the oldest release whose pages are served. |
| `flock.Require(ctx, name)` | What a handler asks before it serves a feature. |
| `geese.ErrGrounded`, `ErrInactive` | Why something was refused. Each means "not now, try again": map them to the transport's way of saying so. |

Whatever the transport, the rules are the same:

- **Admit before touching the database**, and release when done. Work that isn't admitted isn't cancelled when the goose is grounded, so it may run on after the flock stops counting the goose.
- **Refuse, don't fake.** An error from `Admit` or `Require` is answered as "unavailable, try again", never with a made-up result.
- **Clients keep [the contract](capabilities.md#on-the-client)**: what they're told in the transport's terms.

## HTTP: package geesehttp

```go
mux := http.NewServeMux()
mux.Handle("GET /api/capabilities", geesehttp.Capabilities(flock))
mux.Handle("POST /api/posts/{id}/comments", geesehttp.Require(flock, "comments", addComment))
server := &http.Server{Handler: geesehttp.Guard(flock, mux)}
```

`Guard` admits every request, WebSocket upgrades and streams of server-sent events included. Refusals are `503` with `Retry-After`; a feature that isn't on is `{"code": "feature_unavailable", ...}`.

## gRPC

Interceptors, a health service, and the same errors as `UNAVAILABLE`, which gRPC clients may retry. These are a few lines to keep in the application, so geese doesn't pull gRPC into everyone's module:

```go
// admit admits every unary call.
func admit(flock *geese.Flock) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		ctx, release, err := flock.Admit(ctx)
		if err != nil {
			return nil, unavailable(err)
		}
		defer release()
		return handler(ctx, req)
	}
}

// admitStream admits every streaming call.
func admitStream(flock *geese.Flock) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		ctx, release, err := flock.Admit(ss.Context())
		if err != nil {
			return unavailable(err)
		}
		defer release()
		return handler(srv, admitted{ss, ctx})
	}
}

type admitted struct {
	grpc.ServerStream
	ctx context.Context
}

func (s admitted) Context() context.Context { return s.ctx }

// unavailable is UNAVAILABLE, with why: grounded, or a feature that isn't on
// yet.
func unavailable(err error) error {
	reason := "UNAVAILABLE"
	switch {
	case errors.Is(err, geese.ErrInactive):
		reason = "FEATURE_UNAVAILABLE"
	case errors.Is(err, geese.ErrGrounded):
		reason = "GROUNDED"
	}
	st, detailErr := status.New(codes.Unavailable, err.Error()).WithDetails(&errdetails.ErrorInfo{Reason: reason, Domain: "geese"})
	if detailErr != nil {
		return status.Error(codes.Unavailable, err.Error())
	}
	return st.Err()
}
```

Wire them in, with the standard health service following the goose:

```go
server := grpc.NewServer(grpc.ChainUnaryInterceptor(admit(flock)), grpc.ChainStreamInterceptor(admitStream(flock)))
checks := health.NewServer()
healthpb.RegisterHealthServer(server, checks)
go func() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		state := healthpb.HealthCheckResponse_NOT_SERVING
		if flock.Ready() {
			state = healthpb.HealthCheckResponse_SERVING
		}
		checks.SetServingStatus("", state)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}()
```

A handler of a feature that waits for its capability:

```go
func (s *server) AddComment(ctx context.Context, req *pb.AddCommentRequest) (*pb.Comment, error) {
	if err := s.flock.Require(ctx, "comments"); err != nil {
		return nil, unavailable(err)
	}
	// ...
}
```

And clients read the capabilities with an RPC of the service's own:

```proto
service Deployment {
  // What's switched on, and the oldest release whose clients are served.
  rpc GetCapabilities(GetCapabilitiesRequest) returns (Capabilities);
}
message GetCapabilitiesRequest {}
message Capabilities {
  repeated string capabilities = 1;
  // The oldest release whose clients are served, as "8.0.0"; empty until set.
  string oldest_page = 2;
}
```

```go
func (s *server) GetCapabilities(ctx context.Context, _ *pb.GetCapabilitiesRequest) (*pb.Capabilities, error) {
	said, err := s.flock.Capabilities(ctx)
	if err != nil {
		return nil, unavailable(err)
	}
	return &pb.Capabilities{Capabilities: said.Active, OldestPage: said.OldestPage}, nil
}
```

In the client contract, gRPC's terms are: a change is any call that isn't a read; "an answer it didn't expect" is `UNIMPLEMENTED` (the method is gone); "not yet" is `UNAVAILABLE` with the reason `FEATURE_UNAVAILABLE`.

## Anything else

A queue consumer, a scheduled job or a socket server admits each unit of work the same way, and answers a refusal in its own terms: a message left unacknowledged for redelivery, a job retried on its next tick, a socket closed with a code telling its client to come back.

```go
ctx, release, err := flock.Admit(ctx)
if err != nil {
	return err // nack: redelivered to a goose that has its place
}
defer release()
```
