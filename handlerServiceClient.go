package ssrpanel

import (
	"context"
	"google.golang.org/grpc"
	"github.com/v2fly/v2ray-core/v4/app/proxyman/command"
	"github.com/v2fly/v2ray-core/v4/common/protocol"
	"github.com/v2fly/v2ray-core/v4/common/serial"
)

type HandlerServiceClient struct {
	command.HandlerServiceClient
	inboundTag string
}

func NewHandlerServiceClient(client *grpc.ClientConn, inboundTag string) *HandlerServiceClient {
	return &HandlerServiceClient{
		HandlerServiceClient: command.NewHandlerServiceClient(client),
		inboundTag:           inboundTag,
	}
}

func (h *HandlerServiceClient) DelUser(email string) error {
	req := &command.AlterInboundRequest{
		Tag:       h.inboundTag,
		Operation: serial.ToTypedMessage(&command.RemoveUserOperation{Email: email}),
	}
	return h.AlterInbound(req)
}

func (h *HandlerServiceClient) AddUser(user *protocol.User) error {
	req := &command.AlterInboundRequest{
		Tag:       h.inboundTag,
		Operation: serial.ToTypedMessage(&command.AddUserOperation{User: user}),
	}
	return h.AlterInbound(req)
}

func (h *HandlerServiceClient) AlterInbound(req *command.AlterInboundRequest) error {
	_, err := h.HandlerServiceClient.AlterInbound(context.Background(), req)
	return err
}
