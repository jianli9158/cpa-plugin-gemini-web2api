package config

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MinLogTotalMB 是日志目录总量上限的最小值，容纳当前文件与一个轮转文件
const MinLogTotalMB = 20

// PredefinedGemsSelector 在 HIDDEN_MODELS 中表示官网 CNgdBe 类型 3 的全部 Google 预置 Gem
const PredefinedGemsSelector = "predefined-gems"

// Defaults 是默认配置项参考值
var Defaults = map[string]string{
	"ACCOUNT_COOLDOWN": "2m", "SESSION_TTL": "30m", "INIT_TIMEOUT": "20s", "MODEL_MAPPING": "", "HIDDEN_MODELS": "gemini-canvas,gemini-guided-learning," + PredefinedGemsSelector,
	"GEMINI_SAVE_HISTORY": "false", "GEMINI_CONTINUE_SESSION": "false",
	"LOG_LEVEL": "info", "LOG_FORMAT": "text", "REQUEST_BODY_LOG": "false", "LOG_FILE": "true", "LOG_MAX_TOTAL_MB": "500",
	"PER_ACCOUNT_CONCURRENCY": "1", "ROUTING_STRATEGY": "round-robin",
	"FIRST_EVENT_HEDGE": "auto", "FIRST_EVENT_HEDGE_MODELS": "gemini-4-carbon",
	"NONSTREAM_KEEPALIVE": "true", "FAKE_STREAM": "false",
	"ACCOUNT_ORDER": "", "MODEL_ACCOUNTS": "",
}

// Config 保存插件运行期所需的全部配置
type Config struct {
	AuthStatePaths  []string
	ProxyURL        string
	AccountCooldown time.Duration
	SessionTTL      time.Duration
	InitTimeout     time.Duration
	ModelMapping map[string]string
	// HiddenModels 是在公开 API 中关闭的模型 ID 或 predefined-gems 列表
	HiddenModels []string
	SaveHistory  bool
	// ContinueSession 为真时后续请求续接已登记的网页会话，为假时每个请求新建网页会话
	ContinueSession bool
	// NonstreamKeepalive 为真时非流式请求等待结果期间每 10 秒写出一个换行
	NonstreamKeepalive bool
	// FakeStream 为真时流式请求收齐完整结果后再按流式输出
	FakeStream            bool
	LogLevel              string
	LogFormat             string
	RequestBodyLog        bool
	LogFile               bool
	LogMaxTotalMB         int
	PerAccountConcurrency int
	RoutingStrategy       string
	AccountOrder          []string
	ModelAccounts         map[string][]string
	FirstEventHedge       string
	FirstEventHedgeModels []string
}
var (
	modelMapping = map[string]string{}
	mappingMu    sync.RWMutex

	hiddenMu        sync.RWMutex
	hiddenModelList []string
	hiddenModelSet  map[string]bool
	hidePredefined  bool
)

func init() {
	SetHiddenModels(NormalizeHiddenModels(Defaults["HIDDEN_MODELS"]))
}

// NormalizeHiddenModels 解析逗号分隔的隐藏模型名单，规范为小写、去前缀与去重排序后的切片
func NormalizeHiddenModels(value string) []string {
	items := SplitList(value)
	result := make([]string, 0, len(items))
	for _, item := range items {
		normalized := normalizeHiddenEntry(item)
		if normalized != "" {
			result = append(result, normalized)
		}
	}
	slices.Sort(result)
	return slices.Compact(result)
}

// FormatHiddenModels 返回隐藏模型名单写入配置的规范逗号分隔字符串
func FormatHiddenModels(models []string) string {
	return strings.Join(NormalizeHiddenModels(strings.Join(models, ",")), ",")
}

// SetHiddenModels 替换当前生效的隐藏模型名单
func SetHiddenModels(models []string) {
	normalized := NormalizeHiddenModels(strings.Join(models, ","))
	set := make(map[string]bool, len(normalized))
	predefined := false
	for _, item := range normalized {
		if item == PredefinedGemsSelector {
			predefined = true
			continue
		}
		set[item] = true
	}
	hiddenMu.Lock()
	hiddenModelList = normalized
	hiddenModelSet = set
	hidePredefined = predefined
	hiddenMu.Unlock()
}

// HiddenModels 返回当前生效的隐藏模型名单副本
func HiddenModels() []string {
	hiddenMu.RLock()
	defer hiddenMu.RUnlock()
	return slices.Clone(hiddenModelList)
}

// IsModelHidden 判断指定模型 ID 或 Google 预置 Gem 是否在设置中关闭
func IsModelHidden(id string, predefinedGem bool) bool {
	key := normalizeHiddenEntry(id)
	hiddenMu.RLock()
	defer hiddenMu.RUnlock()
	if predefinedGem && hidePredefined {
		return true
	}
	return key != "" && hiddenModelSet[key]
}

// normalizeHiddenEntry 去掉首尾空白、models/ 或 google/ 前缀与 [1m] 后缀并转为小写
func normalizeHiddenEntry(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.TrimSuffix(value, "[1m]")
	value = strings.TrimPrefix(value, "models/")
	return strings.TrimPrefix(value, "google/")
}

// SetModelMapping 替换当前模型别名映射
func SetModelMapping(mapping map[string]string) {
	mappingMu.Lock()
	defer mappingMu.Unlock()

	modelMapping = maps.Clone(mapping)
}

// ModelAliases 返回当前模型别名映射的副本
func ModelAliases() map[string]string {
	mappingMu.RLock()
	defer mappingMu.RUnlock()
	return maps.Clone(modelMapping)
}

// MapModel 去掉首尾空白、models/ 与 google/ 前缀和 Claude Code 1M 上下文模式的 [1m] 后缀后返回别名对应的真实模型标识
func MapModel(model string) string {
	model = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(model), "models/"), "google/"), "[1m]")
	mappingMu.RLock()
	defer mappingMu.RUnlock()

	if mapped, ok := modelMapping[model]; ok {
		return mapped
	}
	return model
}

// SplitList 按逗号拆分列表配置，去掉各项首尾空白与空项
func SplitList(value string) []string {
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			result = append(result, part)
		}
	}
	return result
}

// ValidateFirstEventHedge 检查首事件备用请求方式是 off、auto 或正整数秒
func ValidateFirstEventHedge(value string) error {
	if value == "off" || value == "auto" {
		return nil
	}
	if seconds, err := strconv.Atoi(value); err != nil || seconds < 1 {
		return fmt.Errorf("取 off、auto 或正整数秒")
	}
	return nil
}

// ParseModelAccounts 解析逗号分隔的“模型 ID:账号|账号”绑定，无效项与重复的模型跳过并返回错误，其余绑定照常返回
func ParseModelAccounts(value string) (map[string][]string, error) {
	accounts := make(map[string][]string)
	var problem error
	for _, item := range SplitList(value) {
		model, ids, ok := accountBinding(item)
		if !ok {
			problem = fmt.Errorf("每一项写成 模型 ID:账号|账号")
			continue
		}
		if _, exists := accounts[model]; exists {
			problem = fmt.Errorf("同一模型重复")
			continue
		}
		accounts[model] = ids
	}
	return accounts, problem
}

// accountBinding 以最后一个冒号拆分“名称:账号|账号”，名称或账号为空时 ok 为假
func accountBinding(item string) (name string, ids []string, ok bool) {
	index := strings.LastIndexByte(item, ':')
	if index < 0 {
		return "", nil, false
	}
	for id := range strings.SplitSeq(item[index+1:], "|") {
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
	}
	name = strings.TrimSpace(item[:index])
	return name, ids, name != "" && len(ids) > 0
}

// ParseModelMapping 解析逗号分隔的“别名:模型 ID”映射
func ParseModelMapping(value string) (map[string]string, error) {
	mapping := make(map[string]string)
	for _, pair := range SplitList(value) {
		parts := strings.SplitN(pair, ":", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("MODEL_MAPPING 项 %q 缺少冒号", pair)
		}

		source := strings.TrimSpace(parts[0])
		target := strings.TrimSpace(parts[1])
		if source == "" || target == "" {
			return nil, fmt.Errorf("MODEL_MAPPING 项 %q 的模型名不能为空", pair)
		}
		mapping[source] = target
	}
	return mapping, nil
}
