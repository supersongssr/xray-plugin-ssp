package ssrpanel

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/common/platform"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/infra/conf"
	json_reader "github.com/xtls/xray-core/infra/conf/json"
	"github.com/xtls/xray-core/main/confloader"
)

var (
	cmdLine = flag.NewFlagSet(os.Args[0]+"-plugin-ssp", flag.ContinueOnError)

	configFile = cmdLine.String("config", "", "Config file for V2Ray.")
	_          = cmdLine.Bool("version", false, "Show current version of V2Ray.")
	test       = cmdLine.Bool("test", false, "Test config file only, without launching V2Ray server.")
	_          = cmdLine.String("format", "json", "Format of input file.")
	_          = cmdLine.Bool("plugin", false, "True to load plugins.")
)

type UserConfig struct {
	InboundTag     string            `json:"inboundTag"`
	InboundTags    []string          `json:"inboundTags"`
	Level          uint32            `json:"level"`
	AlterID        uint32            `json:"alterId"`
	Flow           string            `json:"flow"`
	Flows          map[string]string `json:"flows"`
	SecurityStr    string            `json:"securityConfig"`
	securityConfig *protocol.SecurityConfig
}

func (c *UserConfig) GetFlow(tag string) string {
	if flow, ok := c.Flows[tag]; ok {
		return flow
	}
	return c.Flow
}

func (c *UserConfig) GetTags() []string {
	var rawTags []string
	hasValidInboundTags := false

	// 检查 InboundTags 是否包含有效值
	for _, tag := range c.InboundTags {
		if strings.TrimSpace(tag) != "" {
			hasValidInboundTags = true
			break
		}
	}

	if hasValidInboundTags {
		// 如果 InboundTags 有效，完全忽略 InboundTag
		rawTags = c.InboundTags
	} else if strings.TrimSpace(c.InboundTag) != "" {
		// 否则回退到 InboundTag
		rawTags = []string{c.InboundTag}
	}

	// 去重且过滤空字符串
	tagsMap := make(map[string]struct{})
	for _, tag := range rawTags {
		t := strings.TrimSpace(tag)
		if t != "" {
			tagsMap[t] = struct{}{}
		}
	}

	tags := make([]string, 0, len(tagsMap))
	for tag := range tagsMap {
		tags = append(tags, tag)
	}
	return tags
}

func (c *UserConfig) UnmarshalJSON(data []byte) error {
	type config UserConfig
	var cfg config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return err
	}

	// set default value
	if cfg.SecurityStr == "" {
		cfg.SecurityStr = "AUTO"
	}

	// VLESS Flow compatibility: map old flows to new flows (xray-core v1.8.4 -> v26)
	// Old xtls-rprx-direct/splice are deprecated
	mapFlow := func(f string) string {
		switch f {
		case "xtls-rprx-direct", "xtls-rprx-splice":
			return "" // Deprecated, map to empty
		case "xtls-rprx-vision":
			return "xtls-rprx-vision"
		default:
			return f
		}
	}

	cfg.Flow = mapFlow(cfg.Flow)
	for tag, flow := range cfg.Flows {
		cfg.Flows[tag] = mapFlow(flow)
	}

	cfg.securityConfig = &protocol.SecurityConfig{
		Type: protocol.SecurityType(protocol.SecurityType_value[strings.ToUpper(cfg.SecurityStr)]),
	}
	*c = UserConfig(cfg)
	return nil
}

type Config struct {
	NodeID             uint         `json:"nodeId"`
	CheckRate          int          `json:"checkRate"`
	IPLimit            int64        `json:"ipLimit"`
	MySQL              *MySQLConfig `json:"mysql"`
	UserConfig         *UserConfig  `json:"user"`
	IgnoreEmptyVmessID bool         `json:"ignoreEmptyVmessID"`
	// NodeClass 		   string       `json:"NodeClass"`
	v2rayConfig *conf.Config
}

func getConfig() (*Config, error) {
	type config struct {
		*conf.Config
		SSRPanel *Config `json:"ssrpanel"`
	}

	configFile := getConfigFilePath()
	configInput, err := confloader.LoadConfig(configFile)
	if err != nil {
		return nil, errors.New("failed to load config: ", configFile).Base(err)
	}
	// defer configInput.Close()

	cfg := &config{}
	if err = decodeCommentJSON(configInput, cfg); err != nil {
		return nil, err
	}
	if cfg.SSRPanel != nil {
		cfg.SSRPanel.v2rayConfig = cfg.Config
		if err = checkCfg(cfg.SSRPanel); err != nil {
			return nil, err
		}
	}

	return cfg.SSRPanel, err
}

func checkCfg(cfg *Config) error {

	if cfg.v2rayConfig.API == nil {
		return errors.New("Api must be set")
	}

	apiTag := cfg.v2rayConfig.API.Tag
	if len(apiTag) == 0 {
		return errors.New("Api tag can't be empty")
	}

	services := cfg.v2rayConfig.API.Services
	if !InStr("HandlerService", services) {
		return errors.New("Api service, HandlerService, must be enabled")
	}
	if !InStr("StatsService", services) {
		return errors.New("Api service, StatsService, must be enabled")
	}

	if cfg.v2rayConfig.Stats == nil {
		return errors.New("Stats must be enabled")
	}

	if apiInbound := getInboundConfigByTag(apiTag, cfg.v2rayConfig.InboundConfigs); apiInbound == nil {
		return errors.New(fmt.Sprintf("Miss an inbound tagged %s", apiTag))
	} else if apiInbound.Protocol != "dokodemo-door" {
		return errors.New(fmt.Sprintf("The protocol of inbound tagged %s must be \"dokodemo-door\"", apiTag))
	} else {
		if apiInbound.ListenOn == nil || apiInbound.PortList == nil {
			return errors.New(fmt.Sprintf("Fields, \"listen\" and \"port\", of inbound tagged %s must be set", apiTag))
		}
	}

	tags := cfg.UserConfig.GetTags()
	if len(tags) == 0 {
		return errors.New("At least one inboundTag must be set")
	}

	for _, tag := range tags {
		if inbound := getInboundConfigByTag(tag, cfg.v2rayConfig.InboundConfigs); inbound == nil {
			return errors.New(fmt.Sprintf("Miss an inbound tagged %s", tag))
		}
	}

	return nil
}

func getInboundConfigByTag(apiTag string, inbounds []conf.InboundDetourConfig) *conf.InboundDetourConfig {
	for _, inbound := range inbounds {
		if inbound.Tag == apiTag {
			return &inbound
		}
	}
	return nil
}

func getConfigFilePath() string {
	if len(*configFile) > 0 {
		return *configFile
	}

	if workingDir, err := os.Getwd(); err == nil {
		configFile := filepath.Join(workingDir, "config.json")
		if fileExists(configFile) {
			return configFile
		}
	}

	if configFile := platform.GetConfigurationPath(); fileExists(configFile) {
		return configFile
	}

	return ""
}

func decodeCommentJSON(reader io.Reader, i interface{}) error {
	jsonContent := bytes.NewBuffer(make([]byte, 0, 10240))
	jsonReader := io.TeeReader(&json_reader.Reader{
		Reader: reader,
	}, jsonContent)
	decoder := json.NewDecoder(jsonReader)
	return decoder.Decode(i)
}

func fileExists(file string) bool {
	info, err := os.Stat(file)
	return err == nil && !info.IsDir()
}
