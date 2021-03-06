package web

import (
	"context"

	grpc_middleware "github.com/grpc-ecosystem/go-grpc-middleware"
	grpc_auth "github.com/grpc-ecosystem/go-grpc-middleware/auth"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	api "ledger/api/v1"
)

// ACL policy keywords
const (
	objectWildcard = "*"
	produceAction  = "produce"
	consumeAction  = "consume"
)

var _ api.LogServer = (*grpcServer)(nil)

type Config struct {
	CommitLog    CommitLog
	Authorizer   Authorizer
	ServerGetter ServerGetter
}

type CommitLog interface {
	Append(*api.Record) (uint64, error)
	Read(uint64) (*api.Record, error)
}

type Authorizer interface {
	Authorize(subject, object, action string) error
}

type ServerGetter interface {
	GetServers() ([]*api.Server, error)
}

type grpcServer struct {
	*Config
}

func NewGRPCServer(config *Config, opts ...grpc.ServerOption) (*grpc.Server, error) {
	if config.Authorizer != nil {
		opts = append(opts,
			grpc.StreamInterceptor(grpc_middleware.ChainStreamServer(grpc_auth.StreamServerInterceptor(identify))),
			grpc.UnaryInterceptor(grpc_middleware.ChainUnaryServer(grpc_auth.UnaryServerInterceptor(identify))),
		)
	}
	server := grpc.NewServer(opts...)

	logServer := &grpcServer{config}

	api.RegisterLogServer(server, logServer)
	return server, nil
}

func (this *grpcServer) Produce(ctx context.Context, req *api.ProduceRequest) (*api.ProduceResponse, error) {
	if this.Authorizer != nil {
		err := this.Authorizer.Authorize(subject(ctx), objectWildcard, consumeAction)
		if err != nil {
			return nil, err
		}
	}

	offset, err := this.CommitLog.Append(req.Record)
	if err != nil {
		return nil, err
	}

	return &api.ProduceResponse{Offset: offset}, nil
}

func (this *grpcServer) Consume(ctx context.Context, req *api.ConsumeRequest) (*api.ConsumeResponse, error) {
	if this.Authorizer != nil {
		err := this.Authorizer.Authorize(subject(ctx), objectWildcard, consumeAction)
		if err != nil {
			return nil, err
		}
	}

	record, err := this.CommitLog.Read(req.Offset)
	if err != nil {
		return nil, err
	}

	return &api.ConsumeResponse{
		Record: record,
	}, nil
}

func (this *grpcServer) ProduceStream(stream api.Log_ProduceStreamServer) error {
	for {
		// get an incoming
		req, err := stream.Recv()
		if err != nil {
			return err
		}

		res, err := this.Produce(stream.Context(), req)
		if err != nil {
			return err
		}

		err = stream.Send(res)
		if err != nil {
			return err
		}
	}
}

func (this *grpcServer) ConsumeStream(req *api.ConsumeRequest, stream api.Log_ConsumeStreamServer) error {
	for {
		select {
		case <-stream.Context().Done():
			return nil

		// start reading logs starting at req.Offset
		// will stream every record that follows
		// when there are no more logs to read, the server will wait til another record is appended
		default:
			res, err := this.Consume(stream.Context(), req)
			switch err.(type) {
			case nil:
			case api.ErrOffsetOutOfRange:
				continue
			default:
				return err
			}

			err = stream.Send(res)
			if err != nil {
				return err
			}

			req.Offset += 1
		}

	}
}

func (s *grpcServer) GetServers(ctx context.Context, req *api.GetServersRequest) (*api.GetServersResponse, error) {
	servers, err := s.ServerGetter.GetServers()
	if err != nil {
		return nil, err
	}

	return &api.GetServersResponse{Servers: servers}, nil
}

// Identify the subject to enable authorization
// Interceptor/middleware reads subject out of the client's cert and writes it to the RPC's context
func identify(ctx context.Context) (context.Context, error) {
	peer, ok := peer.FromContext(ctx)
	if !ok {
		return ctx, status.New(codes.Unknown, "couldn't find peer info").Err()
	}

	if peer.AuthInfo == nil {
		return ctx, status.New(codes.Unauthenticated, "no transport security used").Err()
	}

	tlsInfo := peer.AuthInfo.(credentials.TLSInfo)
	subject := tlsInfo.State.VerifiedChains[0][0].Subject.CommonName
	ctx = context.WithValue(ctx, subjectContextKey{}, subject)

	return ctx, nil
}

func subject(ctx context.Context) string {
	return ctx.Value(subjectContextKey{}).(string)
}

type subjectContextKey struct{}
