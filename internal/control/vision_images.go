package control

import (
	"context"
	"encoding/base64"
	"fmt"
	"path"
	"path/filepath"
	"strings"
	"time"

	"reasonix/internal/config"
	"reasonix/internal/provider"
	"reasonix/internal/provider/openai"
)

var (
	inlineImageLimit = provider.MaxInlineImageBytes
	uploadVisionFile = provider.UploadUserDataFile
)

func (c *Controller) visionLocalImageValue(pathName, baseDir string) (string, error) {
	var (
		dataURL string
		err     error
	)
	if isAttachmentRef(filepath.ToSlash(pathName)) {
		dataURL, err = visionImageDataURL(pathName)
	} else {
		dataURL, err = visionFileImageDataURL(pathName, baseDir)
	}
	if err != nil {
		return "", err
	}
	return c.maybeUploadDataURL(pathName, dataURL)
}

func (c *Controller) maybeUploadDataURL(filename, dataURL string) (string, error) {
	_, payload, ok := provider.ParseImageDataURL(dataURL)
	if !ok {
		return dataURL, nil
	}
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil || len(raw) <= inlineImageLimit {
		return dataURL, nil
	}
	id, err := c.uploadOfficialVisionFile(filename, raw)
	if err == nil {
		return id, nil
	}
	if len(raw) > provider.MaxInlineImageBytes {
		return "", err
	}
	return dataURL, nil
}

func (c *Controller) uploadOfficialVisionFile(filename string, data []byte) (string, error) {
	if c == nil {
		return "", fmt.Errorf("files api: no session")
	}
	cfg, err := config.LoadForRoot(c.workspaceRoot)
	if err != nil {
		return "", err
	}
	ref := c.modelRef
	if ref == "" {
		ref = cfg.DefaultModel
	}
	entry, ok := cfg.ResolveModel(ref)
	if !ok || !openai.IsDeepSeek(entry.BaseURL) {
		return "", fmt.Errorf("files api requires official DeepSeek")
	}
	protocol := "openai"
	if strings.EqualFold(entry.Kind, "anthropic") {
		protocol = "anthropic"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	return uploadVisionFile(ctx, provider.FileUpload{
		BaseURL:    entry.BaseURL,
		APIKey:     entry.APIKey(),
		AuthHeader: entry.AuthHeader,
		Protocol:   protocol,
		Filename:   path.Base(filename),
		Data:       data,
	})
}
