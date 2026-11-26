package hostbridge

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"slices"
)

// setting-options.json is optional. Older Cage builds can still load the
// plugin manifest and edit the underlying string setting.
type SettingOptionsSource struct {
	EndpointSetting string   `json:"endpointSetting"`
	TokenSetting    string   `json:"tokenSetting,omitempty"`
	Resource        string   `json:"resource"`
	ItemsPath       []string `json:"itemsPath,omitempty"`
	ValueField      string   `json:"valueField,omitempty"`
	LabelField      string   `json:"labelField,omitempty"`
}

func (m Manifest) loadSettingOptions(dir string) error {
	const file = "setting-options.json"
	f, err := os.OpenInRoot(dir, file)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > 65536 {
		return fmt.Errorf("%s: expected a small JSON file", file)
	}
	data, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil {
		return err
	}
	var sources map[string]SettingOptionsSource
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if !json.Valid(data) || decoder.Decode(&sources) != nil || sources == nil {
		return fmt.Errorf("%s: invalid option sources", file)
	}
	if m.Kind != KindHost || !slices.Contains(m.Permissions, "network") {
		return fmt.Errorf("%s: MCP resource discovery requires a host plugin with network permission", file)
	}
	fields := map[string]*Setting{}
	for i := range m.Contributes.Settings {
		field := &m.Contributes.Settings[i]
		fields[field.ID] = field
	}
	for id, source := range sources {
		field, endpoint, token := fields[id], fields[source.EndpointSetting], fields[source.TokenSetting]
		u, err := url.Parse(source.Resource)
		switch {
		case field == nil || field.Type != SettingString:
			return fmt.Errorf("%s: %q must refer to a string setting", file, id)
		case endpoint == nil || endpoint.Type != SettingString || endpoint.ID == id:
			return fmt.Errorf("%s: %q needs a separate endpoint string setting", file, id)
		case source.TokenSetting != "" && (token == nil || token.Type != SettingSecret):
			return fmt.Errorf("%s: %q needs a secret token setting", file, id)
		case err != nil || !u.IsAbs() || !ValidSettingValue(source.Resource):
			return fmt.Errorf("%s: %q has an invalid resource URI", file, id)
		case source.LabelField != "" && source.ValueField == "":
			return fmt.Errorf("%s: %q needs valueField when using labelField", file, id)
		}
		for _, key := range source.ItemsPath {
			if key == "" {
				return fmt.Errorf("%s: %q has an empty path component", file, id)
			}
		}
		field.OptionsSource = &source
	}
	return nil
}
