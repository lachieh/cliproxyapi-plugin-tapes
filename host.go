package main

import (
	"encoding/json"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// hostBridge implements usage.Host over host callbacks.
type hostBridge struct{}

func (hostBridge) ListAuths() ([]pluginapi.HostAuthFileEntry, error) {
	raw, err := callHost(pluginabi.MethodHostAuthList, map[string]any{})
	if err != nil {
		return nil, err
	}
	var resp struct {
		Files []pluginapi.HostAuthFileEntry `json:"files"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decode host.auth.list: %w", err)
	}
	return resp.Files, nil
}

func (hostBridge) AuthJSON(authIndex string) (json.RawMessage, error) {
	raw, err := callHost(pluginabi.MethodHostAuthGet, pluginapi.HostAuthGetRequest{AuthIndex: authIndex})
	if err != nil {
		return nil, err
	}
	var resp pluginapi.HostAuthGetResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decode host.auth.get: %w", err)
	}
	return resp.JSON, nil
}

func (hostBridge) Do(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	raw, err := callHost(pluginabi.MethodHostHTTPDo, map[string]any{
		"method":  req.Method,
		"url":     req.URL,
		"headers": req.Headers,
		"body":    req.Body,
	})
	if err != nil {
		return pluginapi.HTTPResponse{}, err
	}
	var resp pluginapi.HTTPResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return pluginapi.HTTPResponse{}, fmt.Errorf("decode host.http.do: %w", err)
	}
	return resp, nil
}
