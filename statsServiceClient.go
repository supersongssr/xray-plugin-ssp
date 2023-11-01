package ssrpanel

import (
	"context"
	"fmt"
	"strings"

	statsservice "github.com/xtls/xray-core/app/stats/command"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
)

type StatsServiceClient struct {
	statsservice.StatsServiceClient
}

func NewStatsServiceClient(client *grpc.ClientConn) *StatsServiceClient {
	return &StatsServiceClient{
		StatsServiceClient: statsservice.NewStatsServiceClient(client),
	}
}

func (s *StatsServiceClient) getUserUplink(email string) (uint64, error) {
	return s.getUserTraffic(fmt.Sprintf("user>>>%s>>>traffic>>>uplink", email), true)
}

func (s *StatsServiceClient) getUserDownlink(email string) (uint64, error) {
	return s.getUserTraffic(fmt.Sprintf("user>>>%s>>>traffic>>>downlink", email), true)
}

func (s *StatsServiceClient) getUserTraffic(name string, reset bool) (uint64, error) {
	req := &statsservice.GetStatsRequest{
		Name:   name,
		Reset_: reset,
	}

	res, err := s.GetStats(context.Background(), req)
	if err != nil {
		if status, ok := status.FromError(err); ok && strings.HasSuffix(status.Message(), fmt.Sprintf("%s not found.", name)) {
			return 0, nil
		}

		return 0, err
	}

	return uint64(res.Stat.Value), nil
}

// song get user ip
func (s *StatsServiceClient) getUserIP(email string) (int64, string, error) {
	name := fmt.Sprintf("user>>>%s>>>traffic>>>ips", email)
	req := &statsservice.GetStatsRequest{
		Name:   name,
		Reset_: true,
	}
	res, err := s.GetStats(context.Background(), req)
	if err != nil {
		if status, ok := status.FromError(err); ok && strings.HasSuffix(status.Message(), fmt.Sprintf("%s not found.", name)) {
			return 0, "", nil
		}

		return 0, "", err
	}
	return res.Stat.Value, res.Stat.Name, nil
}
