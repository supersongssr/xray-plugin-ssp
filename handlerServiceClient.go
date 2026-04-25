package ssrpanel

import (
	"context"

	"github.com/xtls/xray-core/app/proxyman/command"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/serial"
	"google.golang.org/grpc"
)

type HandlerServiceClient struct {
	command.HandlerServiceClient
}

func NewHandlerServiceClient(client *grpc.ClientConn) *HandlerServiceClient {
	return &HandlerServiceClient{
		HandlerServiceClient: command.NewHandlerServiceClient(client),
	}
}

func (h *HandlerServiceClient) RemoveUser(tag string, email string) error {
	req := &command.AlterInboundRequest{
		Tag:       tag,
		Operation: serial.ToTypedMessage(&command.RemoveUserOperation{Email: email}),
	}
	return h.AlterInbound(req)
}

func (h *HandlerServiceClient) AddUser(tag string, user *protocol.User) error {
	req := &command.AlterInboundRequest{
		Tag:       tag,
		Operation: serial.ToTypedMessage(&command.AddUserOperation{User: user}),
	}
	return h.AlterInbound(req)
}

func (h *HandlerServiceClient) AlterInbound(req *command.AlterInboundRequest) error {
	_, err := h.HandlerServiceClient.AlterInbound(context.Background(), req)
	return err
}
