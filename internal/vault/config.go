package vault

import "strings"

// Config is the Tigris connection. Fly sets BUCKET_NAME, AWS_ENDPOINT_URL_S3,
// AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY and AWS_REGION on the app; local
// tools use the same names behind the REPLAY_VAULT_ prefix.
type Config struct {
	Bucket          string
	Endpoint        string
	AccessKeyID     string
	SecretAccessKey string
	Region          string
	PathStyle       bool
}

// Enabled: with the vault env absent the server behaves as it always did.
func (c Config) Enabled() bool {
	return c.Bucket != "" && c.AccessKeyID != "" && c.SecretAccessKey != ""
}

// ConfigFromEnv reads the unprefixed names (the server on Fly).
func ConfigFromEnv(get func(string) string) Config { return ConfigFromPrefix(get, "") }

// ConfigFromPrefix reads only the names behind prefix, e.g. "REPLAY_VAULT_" for
// local tools, so an unrelated AWS_* in the shell is never picked up.
func ConfigFromPrefix(get func(string) string, prefix string) Config {
	pick := func(name string) string { return strings.TrimSpace(get(prefix + name)) }
	c := Config{
		Bucket:          pick("BUCKET_NAME"),
		Endpoint:        pick("AWS_ENDPOINT_URL_S3"),
		AccessKeyID:     pick("AWS_ACCESS_KEY_ID"),
		SecretAccessKey: pick("AWS_SECRET_ACCESS_KEY"),
		Region:          pick("AWS_REGION"),
		PathStyle:       pick("AWS_S3_PATH_STYLE") == "1",
	}
	if c.Region == "" {
		c.Region = "auto"
	}
	return c
}
