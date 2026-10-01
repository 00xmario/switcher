package claudecode

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"switcher/internal/store"
)

var sharedKeys = []string{"mcpOAuth", "mcpOAuthClientConfig", "mcpXaaIdp", "mcpXaaIdpConfig", "pluginSecrets"}

type Identity struct {
	UUID             string `json:"accountUuid"`
	Email            string `json:"emailAddress"`
	OrganizationUUID string `json:"organizationUuid,omitempty"`
	OrganizationName string `json:"organizationName,omitempty"`
	DisplayName      string `json:"displayName,omitempty"`
	Plan             string `json:"-"`
}

type oauthData struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    int64  `json:"expiresAt"`
}

func object(data []byte) (map[string]json.RawMessage, error) {
	var result map[string]json.RawMessage
	if len(data) == 0 {
		return map[string]json.RawMessage{}, nil
	}
	if json.Unmarshal(data, &result) != nil || result == nil {
		return nil, errors.New("native Claude JSON is malformed; no file was overwritten")
	}
	return result, nil
}

func parseOAuth(data []byte) (oauthData, error) {
	raw, err := object(data)
	if err != nil {
		return oauthData{}, err
	}
	var oauth oauthData
	if _, exists := raw["claudeAiOauth"]; !exists {
		return oauth, nil
	}
	if json.Unmarshal(raw["claudeAiOauth"], &oauth) != nil {
		return oauthData{}, errors.New("native Claude OAuth credential is missing or malformed")
	}
	return oauth, nil
}

func identityOf(config []byte) (Identity, error) {
	obj, err := object(config)
	if err != nil {
		return Identity{}, err
	}
	if _, exists := obj["oauthAccount"]; !exists {
		return Identity{}, nil
	}
	var identity Identity
	if json.Unmarshal(obj["oauthAccount"], &identity) != nil {
		return Identity{}, errors.New("native oauthAccount is malformed")
	}
	return identity, nil
}

func sameIdentity(a, b Identity) bool {
	return a.UUID != "" && a.UUID == b.UUID && strings.EqualFold(a.Email, b.Email) && a.OrganizationUUID == b.OrganizationUUID
}

func owns(identity Identity, a store.Account) bool {
	return identity.UUID != "" && identity.UUID == a.Token.AccountID && strings.EqualFold(identity.Email, a.Email) &&
		a.ClaudeCode != nil && identity.OrganizationUUID == accountIdentity(a).OrganizationUUID
}

func accountIdentity(a store.Account) Identity {
	identity := Identity{UUID: a.Token.AccountID, Email: a.Email}
	if a.ClaudeCode != nil {
		_ = json.Unmarshal(a.ClaudeCode.OAuthAccount, &identity)
	}
	// Account storage remains authoritative over optional imported metadata.
	identity.UUID = a.Token.AccountID
	identity.Email = a.Email
	return identity
}

func family(o oauthData, a store.Account) bool {
	return (o.RefreshToken != "" && o.RefreshToken == a.Token.RefreshToken) || (o.AccessToken != "" && o.AccessToken == a.Token.AccessToken)
}

func applyNative(a *store.Account, credential []byte, identity Identity) error {
	oauth, err := parseOAuth(credential)
	if err != nil {
		return err
	}
	if oauth.AccessToken == "" {
		return errors.New("native token was cleared; the saved account was retained")
	}
	a.Token.AccessToken = oauth.AccessToken
	a.Token.RefreshToken = oauth.RefreshToken
	a.Token.ExpiresAt = oauth.ExpiresAt / 1000
	identityData, err := identityRecord(*a, identity)
	if err != nil {
		return err
	}
	a.ClaudeCode = &store.ClaudeCodeLogin{Credentials: append([]byte(nil), credential...), OAuthAccount: identityData}
	if identity.Plan != "" {
		a.Plan = identity.Plan
	}
	return nil
}

func identityRecord(a store.Account, identity Identity) ([]byte, error) {
	result := map[string]json.RawMessage{}
	if a.ClaudeCode != nil && len(a.ClaudeCode.OAuthAccount) > 0 {
		var err error
		result, err = object(a.ClaudeCode.OAuthAccount)
		if err != nil {
			return nil, err
		}
	}
	known, _ := object(encodedIdentity(identity))
	for _, key := range []string{"accountUuid", "emailAddress", "organizationUuid"} {
		delete(result, key)
		if value, ok := known[key]; ok {
			result[key] = value
		}
	}
	for _, key := range []string{"organizationName", "displayName"} {
		if value, ok := known[key]; ok {
			result[key] = value
		}
	}
	return json.Marshal(result)
}

func encodedIdentity(identity Identity) []byte { raw, _ := json.Marshal(identity); return raw }

func retainIdentityMetadata(a *store.Account, config []byte, identity Identity) {
	configured, err := identityOf(config)
	if err != nil || !sameIdentity(configured, identity) {
		return
	}
	data, err := object(config)
	if err != nil {
		return
	}
	var credential json.RawMessage
	if a.ClaudeCode != nil {
		credential = a.ClaudeCode.Credentials
	}
	a.ClaudeCode = &store.ClaudeCodeLogin{Credentials: credential, OAuthAccount: append([]byte(nil), data["oauthAccount"]...)}
}

func credentialsFor(a store.Account, live []byte) ([]byte, error) {
	data := map[string]json.RawMessage{}
	if a.ClaudeCode != nil {
		var err error
		data, err = object(a.ClaudeCode.Credentials)
		if err != nil {
			return nil, err
		}
	}
	oauth := map[string]json.RawMessage{}
	if len(data["claudeAiOauth"]) > 0 {
		var err error
		oauth, err = object(data["claudeAiOauth"])
		if err != nil {
			return nil, err
		}
	}
	put := func(key string, value any) { raw, _ := json.Marshal(value); oauth[key] = raw }
	put("accessToken", a.Token.AccessToken)
	put("refreshToken", a.Token.RefreshToken)
	put("expiresAt", a.Token.ExpiresAt*1000)
	if _, ok := oauth["scopes"]; !ok {
		put("scopes", []string{"user:profile", "user:inference", "user:sessions:claude_code", "user:mcp_servers", "user:file_upload"})
	}
	if _, ok := oauth["subscriptionType"]; !ok {
		if strings.Contains(a.Plan, "max") {
			put("subscriptionType", "max")
		} else if strings.Contains(a.Plan, "pro") {
			put("subscriptionType", "pro")
		}
	}
	data["claudeAiOauth"], _ = json.Marshal(oauth)
	// Shared keys are wholly live-owned, including absence. Account-bound
	// trustedDeviceToken and unknown fields come only from the target snapshot.
	shared, err := object(live)
	if err != nil {
		return nil, err
	}
	for _, key := range sharedKeys {
		delete(data, key)
		if value, ok := shared[key]; ok {
			data[key] = value
		}
	}
	return json.Marshal(data)
}

func fresh(a store.Account, margin time.Duration) bool {
	return a.Token.AccessToken != "" && a.Token.ExpiresAt > time.Now().Add(margin).Unix()
}
func intText(value int) string { return strconv.Itoa(value) }
func identityError() error {
	return fmt.Errorf("Claude credential identity could not be verified; login in Claude Code and import that login before switching")
}
