package cpa

import (
	"encoding/json"
	"strings"

	"github.com/Mag1cFall/Gemini-Web2API/internal/adapter"
	"github.com/Mag1cFall/Gemini-Web2API/internal/config"
	"github.com/Mag1cFall/Gemini-Web2API/internal/gemini"
	"github.com/Mag1cFall/Gemini-Web2API/internal/pluginapi"
)

type rpcAuthModelRequest struct {
	pluginapi.AuthModelRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type rpcModelRouteRequest struct {
	pluginapi.ModelRouteRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

func (p *Plugin) Config() PluginConfig {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.pluginCfg
}

func (p *Plugin) handleModelStatic(raw []byte) (any, error) {
	var req pluginapi.StaticModelRequest
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &req)
	}
	models := p.collectCatalogModelsForAccount("")
	return pluginapi.ModelResponse{
		Provider: ProviderID,
		Models:   models,
	}, nil
}

func (p *Plugin) handleModelForAuth(raw []byte) (any, error) {
	var req rpcAuthModelRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}

	if !isGeminiWebProvider(req.AuthProvider) {
		var payload AuthFilePayload
		if len(req.StorageJSON) > 0 {
			_ = json.Unmarshal(req.StorageJSON, &payload)
		}
		if !isGeminiWebProvider(payload.Type) && !isGeminiWebProvider(payload.Provider) {
			return pluginapi.ModelResponse{}, nil
		}
	}

	var payload AuthFilePayload
	if len(req.StorageJSON) > 0 {
		_ = json.Unmarshal(req.StorageJSON, &payload)
	}
	accID := extractAccountIDFromAuth(req.StorageJSON, req.Metadata, req.Attributes)
	var authUpdate pluginapi.AuthData
	if state, ok := extractStorageStateFromPayload(req.StorageJSON, payload); ok {
		if rt, err := p.Runtime(); err == nil {
			if importedID, err := p.upsertAccountInRuntime(rt, accID, payload, state); err == nil && importedID != "" {
				accID = importedID
				if updated, err := p.buildAuthDataForAccountID(rt, importedID); err == nil {
					if req.AuthID != "" {
						updated.ID = req.AuthID
					}
					authUpdate = updated
				}
			}
		}
	}

	models := p.collectCatalogModelsForAccount(accID)
	return pluginapi.ModelResponse{
		Provider:   ProviderID,
		Models:     models,
		AuthUpdate: authUpdate,
	}, nil
}

func (p *Plugin) handleModelRoute(raw []byte) (any, error) {
	var req rpcModelRouteRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	model := strings.TrimSpace(req.RequestedModel)
	if model == "" || p.serviceStopped.Load() {
		return pluginapi.ModelRouteResponse{Handled: false}, nil
	}

	prefix := strings.TrimSpace(p.Config().ModelPrefix)
	if prefix == "" {
		prefix = "gemini-web/"
	}

	lowerModel := strings.ToLower(model)
	for _, pfx := range []string{strings.ToLower(prefix), "gemini-web/", "gemini-web2api/"} {
		if pfx != "" && strings.HasPrefix(lowerModel, pfx) {
			clean := strings.TrimSpace(model[len(pfx):])
			if clean == "" {
				continue
			}
			return pluginapi.ModelRouteResponse{
				Handled:     true,
				TargetKind:  pluginapi.ModelRouteTargetSelf,
				Target:      PluginID,
				TargetModel: clean,
				Reason:      "routed prefixed Gemini Web model to gemini-web2api",
			}, nil
		}
	}

	if p.hasReadyAccount() && p.isKnownModel(model) {
		hasBuiltinGemini := false
		for _, prov := range req.AvailableProviders {
			pLower := strings.ToLower(strings.TrimSpace(prov))
			if pLower == "gemini" || pLower == "aistudio" || pLower == "aistudio2api" || pLower == "vertex" {
				hasBuiltinGemini = true
				break
			}
		}
		if !hasBuiltinGemini {
			return pluginapi.ModelRouteResponse{
				Handled:     true,
				TargetKind:  pluginapi.ModelRouteTargetSelf,
				Target:      PluginID,
				TargetModel: model,
				Reason:      "routed Gemini Web model to gemini-web2api",
			}, nil
		}
	}

	return pluginapi.ModelRouteResponse{Handled: false}, nil
}

func (p *Plugin) hasReadyAccount() bool {
	if p.serviceStopped.Load() {
		return false
	}
	rt, err := p.Runtime()
	if err != nil {
		return false
	}
	return rt.Pool.Status().Ready()
}

func (p *Plugin) isKnownModel(modelID string) bool {
	clean := StripModelPrefix(modelID, p.Config().ModelPrefix)
	clean = strings.TrimPrefix(clean, "models/")
	if clean == "" {
		return false
	}
	mapped := config.MapModel(clean)
	for _, m := range p.collectCatalogModelsForAccount("") {
		id := strings.TrimPrefix(strings.TrimSpace(m.ID), "models/")
		if strings.EqualFold(id, clean) || strings.EqualFold(id, mapped) {
			return true
		}
	}
	return false
}

func StripModelPrefix(model string, configuredPrefix string) string {
	trimmed := strings.TrimSpace(model)
	lower := strings.ToLower(trimmed)
	for _, pfx := range []string{strings.ToLower(strings.TrimSpace(configuredPrefix)), "gemini-web/", "gemini-web2api/"} {
		if pfx != "" && strings.HasPrefix(lower, pfx) {
			return strings.TrimSpace(trimmed[len(pfx):])
		}
	}
	return trimmed
}

func (p *Plugin) collectCatalogModelsForAccount(accountID string) []pluginapi.ModelInfo {
	var rawModels []adapter.CatalogModel
	if rt, err := p.Runtime(); err == nil {
		if accountID != "" && accountID != gemini.GuestID {
			if client := rt.Pool.Client(accountID); client != nil {
				for _, m := range client.Models() {
					rawModels = append(rawModels, adapter.CatalogModel{
						Model:              m,
						MinInputTokenLimit: client.ContextWindow(),
						MaxInputTokenLimit: client.ContextWindow(),
						AccountCount:       1,
						Accounts:           []string{accountID},
					})
				}
			}
		}
		if len(rawModels) == 0 {
			rawModels = adapter.Catalog(rt.Pool)
		}
	}
	if len(rawModels) == 0 {
		rawModels = defaultFallbackModels()
	}

	out := make([]pluginapi.ModelInfo, 0, len(rawModels))
	seen := make(map[string]struct{}, len(rawModels))
	addModel := func(info pluginapi.ModelInfo) {
		key := strings.ToLower(strings.TrimSpace(info.ID))
		if key == "" {
			return
		}
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}
		out = append(out, info)
	}

	for _, cm := range rawModels {
		info := convertCatalogModel(cm)
		if info.ID != "" {
			addModel(info)
		}
	}
	return out
}

func convertCatalogModel(cm adapter.CatalogModel) pluginapi.ModelInfo {
	id := strings.TrimPrefix(strings.TrimSpace(cm.ID), "models/")
	if id == "" {
		return pluginapi.ModelInfo{}
	}
	displayName := strings.TrimSpace(cm.DisplayName)
	if displayName == "" {
		displayName = id
	}
	inLimit := cm.MaxInputTokenLimit
	if inLimit <= 0 {
		inLimit = 1048576
	}
	outLimit := 65536

	methods := []string{"generateContent", "streamGenerateContent", "countTokens"}
	inputModalities := []string{"text", "image", "audio", "video", "pdf"}
	outputModalities := []string{"text"}

	switch cm.Feature {
	case gemini.FeatureImage:
		outputModalities = append(outputModalities, "image")
		methods = append(methods, "predict")
	case gemini.FeatureVideo:
		outputModalities = []string{"video"}
		methods = append(methods, "predictLongRunning")
	case gemini.FeatureMusic:
		outputModalities = append(outputModalities, "audio")
	}

	var thinking *pluginapi.ThinkingSupport
	if len(cm.ThinkingLevels) > 0 {
		levels := make([]string, 0, len(cm.ThinkingLevels))
		for _, lvl := range cm.ThinkingLevels {
			levels = append(levels, lvl.String())
		}
		thinking = &pluginapi.ThinkingSupport{
			DynamicAllowed: true,
			Levels:         levels,
			Min:            128,
			Max:            32768,
			ZeroAllowed:    len(levels) > 1,
		}
	}

	return pluginapi.ModelInfo{
		ID:                         id,
		Object:                     "model",
		OwnedBy:                    ProviderID,
		Type:                       ProviderID,
		DisplayName:                displayName,
		Name:                       "models/" + id,
		Description:                cm.Description,
		InputTokenLimit:            int64(inLimit),
		OutputTokenLimit:           int64(outLimit),
		ContextLength:              int64(inLimit),
		MaxCompletionTokens:        int64(outLimit),
		SupportedGenerationMethods: methods,
		SupportedInputModalities:   inputModalities,
		SupportedOutputModalities:  outputModalities,
		Thinking:                   thinking,
		UserDefined:                true,
	}
}

func defaultFallbackModels() []adapter.CatalogModel {
	return []adapter.CatalogModel{
		{
			Model: gemini.Model{
				ID:             "gemini-2.5-pro",
				DisplayName:    "Gemini 2.5 Pro",
				Description:    "Google Gemini Web 2.5 Pro with deep reasoning & multimodal context",
				Capabilities:   []string{"thinking"},
				ThinkingLevels: []gemini.ThinkingLevel{gemini.ThinkingHigh},
			},
			MinInputTokenLimit: 1048576,
			MaxInputTokenLimit: 1048576,
		},
		{
			Model: gemini.Model{
				ID:             "gemini-2.5-flash",
				DisplayName:    "Gemini 2.5 Flash",
				Description:    "Google Gemini Web 2.5 Flash fast multimodal model",
				Capabilities:   []string{"thinking"},
				ThinkingLevels: []gemini.ThinkingLevel{gemini.ThinkingLow, gemini.ThinkingMedium, gemini.ThinkingHigh},
				Default:        true,
			},
			MinInputTokenLimit: 1048576,
			MaxInputTokenLimit: 1048576,
		},
		{
			Model: gemini.Model{
				ID:             "gemini-3-pro",
				DisplayName:    "Gemini 3 Pro",
				Description:    "Google Gemini Web 3 Pro",
				Capabilities:   []string{"thinking"},
				ThinkingLevels: []gemini.ThinkingLevel{gemini.ThinkingHigh},
			},
			MinInputTokenLimit: 1048576,
			MaxInputTokenLimit: 1048576,
		},
		{
			Model: gemini.Model{
				ID:             "gemini-3-flash",
				DisplayName:    "Gemini 3 Flash",
				Description:    "Google Gemini Web 3 Flash",
				Capabilities:   []string{"thinking"},
				ThinkingLevels: []gemini.ThinkingLevel{gemini.ThinkingLow, gemini.ThinkingMedium, gemini.ThinkingHigh},
			},
			MinInputTokenLimit: 1048576,
			MaxInputTokenLimit: 1048576,
		},
		{
			Model: gemini.Model{
				ID:           "gemini-3.1-flash-image",
				DisplayName:  "Nano Banana 2",
				Description:  "Gemini Web image generation and editing",
				Capabilities: []string{"image"},
				Feature:      gemini.FeatureImage,
			},
			MinInputTokenLimit: 32768,
			MaxInputTokenLimit: 32768,
		},
		{
			Model: gemini.Model{
				ID:           "gemini-video",
				DisplayName:  "Veo",
				Description:  "Gemini Web video generation",
				Capabilities: []string{"video"},
				Feature:      gemini.FeatureVideo,
			},
			MinInputTokenLimit: 32768,
			MaxInputTokenLimit: 32768,
		},
		{
			Model: gemini.Model{
				ID:           "gemini-music",
				DisplayName:  "Lyria",
				Description:  "Gemini Web music generation",
				Capabilities: []string{"music"},
				Feature:      gemini.FeatureMusic,
			},
			MinInputTokenLimit: 32768,
			MaxInputTokenLimit: 32768,
		},
		{
			Model: gemini.Model{
				ID:             "gemini-deep-research",
				DisplayName:    "Deep Research",
				Description:    "Gemini Web Deep Research agent",
				Capabilities:   []string{"deep_research", "thinking"},
				ThinkingLevels: []gemini.ThinkingLevel{gemini.ThinkingHigh},
				Feature:        gemini.FeatureDeepResearch,
			},
			MinInputTokenLimit: 1048576,
			MaxInputTokenLimit: 1048576,
		},
		{
			Model: gemini.Model{
				ID:             "gemini-canvas",
				DisplayName:    "Canvas",
				Description:    "Gemini Web Canvas interactive document and code creation",
				Capabilities:   []string{"canvas", "thinking"},
				ThinkingLevels: []gemini.ThinkingLevel{gemini.ThinkingLow, gemini.ThinkingMedium, gemini.ThinkingHigh},
				Feature:        gemini.FeatureCanvas,
			},
			MinInputTokenLimit: 1048576,
			MaxInputTokenLimit: 1048576,
		},
		{
			Model: gemini.Model{
				ID:             "gemini-guided-learning",
				DisplayName:    "Guided Learning",
				Description:    "Gemini Web Guided Learning",
				Capabilities:   []string{"guided_learning", "thinking"},
				ThinkingLevels: []gemini.ThinkingLevel{gemini.ThinkingLow, gemini.ThinkingMedium, gemini.ThinkingHigh},
				Feature:        gemini.FeatureLearning,
			},
			MinInputTokenLimit: 1048576,
			MaxInputTokenLimit: 1048576,
		},
	}
}
