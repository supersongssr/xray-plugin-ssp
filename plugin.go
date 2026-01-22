package ssrpanel

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/xtls/xray-core/common/errors"
	"google.golang.org/grpc/status"
)

func init() {
	go func() {
		err := run()
		if err != nil {
			fatal(err)
		}
		newError("xray v26.1.18 ssp online limit started").AtWarning().WriteToLog()
	}()
}

func run() error {
	cmdLine.Parse(os.Args[2:]) //fixed xray run falg bug

	cfg, err := getConfig()
	if err != nil || *test || cfg == nil {
		return err
	}

	// wait v2ray
	time.Sleep(time.Second)

	db, err := NewMySQLConn(cfg.MySQL)
	if err != nil {
		return err
	}

	go func() {
		apiInbound := getInboundConfigByTag(cfg.v2rayConfig.API.Tag, cfg.v2rayConfig.InboundConfigs)
		gRPCAddr := fmt.Sprintf("%s:%d", apiInbound.ListenOn.String(), apiInbound.PortList.Range[0].From)
		gRPCConn, err := connectGRPC(gRPCAddr, 10*time.Second)
		if err != nil {
			if s, ok := status.FromError(err); ok {
				err = errors.New(s.Message())
			}
			fatal(fmt.Sprintf("connect to gRPC server \"%s\" err: ", gRPCAddr), err)
		}
		newErrorf("Connected gRPC server \"%s\" ", gRPCAddr).AtWarning().WriteToLog()

		p, err := NewPanel(gRPCConn, db, cfg)
		if err != nil {
			fatal("new panel error", err)
		}

		p.Start()
	}()

	return nil
}

func newErrorf(format string, a ...interface{}) *logWriter {
	return &logWriter{err: errors.New(fmt.Sprintf(format, a...))}
}

func newError(values ...interface{}) *logWriter {
	values = append([]interface{}{"PluginSsp: "}, values...)
	return &logWriter{err: errors.New(values...)}
}

// logWriter 兼容旧版本的链式调用 API
type logWriter struct {
	err error
}

func (l *logWriter) AtDebug() *logWriter {
	return l
}

func (l *logWriter) AtInfo() *logWriter {
	return l
}

func (l *logWriter) AtWarning() *logWriter {
	return l
}

func (l *logWriter) AtError() *logWriter {
	return l
}

func (l *logWriter) Base(inner error) *logWriter {
	if inner != nil {
		l.err = fmt.Errorf("%v: %w", l.err, inner)
	}
	return l
}

func (l *logWriter) WriteToLog() {
	ctx := context.Background()
	switch l.err.(type) {
	case *errors.Error:
		errors.LogDebug(ctx, l.err)
	default:
		errors.LogDebug(ctx, l.err)
	}
}

func fatal(values ...interface{}) {
	newError(values...).WriteToLog()
	// Wait log
	time.Sleep(1 * time.Second)
	os.Exit(-2)
}
