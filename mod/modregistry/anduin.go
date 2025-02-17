// Copyright (C) 2014-2024 Anduin Transactions Inc.
//
// Anduin maintained source code to patch OCI client behavior

package modregistry

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"slices"
	"strconv"
	"strings"

	"cuelang.org/go/internal/mod/semver"
	"cuelang.org/go/mod/modfile"
	"cuelang.org/go/mod/module"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	cueModuleAnnotationFileEnv = "ANDUIN_CUE_MODULE_ANNOTATION_FILE"
)

var logging, _ = strconv.ParseBool(os.Getenv("ANDUIN_CUE_DEBUG"))

type anduinPatch struct {
	originalClient *Client
}

// repackZipFile
// Override default put module function to make restructure OCI layer. The goal is to make it compatible with both Oras and Cue cli
// The expected layers are as following:
//  1. All file with annotations of oras
//  2. ZIP file with only *.cue file included (compatible layer with cue)
//  3. Cue module file (compatible layer with cue)
//
// Note: *.cue will be duplicated because Oras will not pull cue layers
// Function should return a list of oras layer descriptors
func (p *anduinPatch) repackZipFile(repackZip *os.File, ctx context.Context, m *checkedModule) ([]ocispec.Descriptor, error) {
	logf("using patched `putCheckedModule`")

	loc, err := p.originalClient.resolve(m.mv)
	if err != nil {
		return nil, err
	}

	zw := zip.NewWriter(repackZip)

	// oras only layers

	logf("valid files: %v", m.validFiles)
	blobPusher := newParallelBlobPusher(ctx, loc)

	for idx, zf := range m.zipr.File {
		// only handle valid file
		if !slices.Contains(m.validFiles, zf.Name) {
			continue
		}

		// sequentialy adding file to zip file
		// but parallel uploading to docker registry
		err := func() error {
			rc, err := zf.Open()
			if err != nil {
				return err
			}
			defer rc.Close()

			// single buffer for multiple copy operation
			// avoid re-reading zip file
			data, err := io.ReadAll(rc)
			if err != nil {
				return err
			}

			// only add related cue file to repack zip
			// this repack must executed sequentialy
			// zip writer is not thread safe
			if shouldRepackFile(zf) {
				if err := addFileToRepack(zw, zf.Name, bytes.NewReader(data)); err != nil {
					return err
				}
			}

			annotation := map[string]string{}
			annotation[ocispec.AnnotationTitle] = zf.Name
			dataLayer := ocispec.Descriptor{
				Digest:      digest.FromBytes(data),
				MediaType:   ocispec.MediaTypeImageLayer,
				Size:        int64(len(data)),
				Annotations: annotation,
			}

			// parallel push blob
			blobPusher.run(idx, &pushBlobRequest{
				desc: dataLayer,
				r:    bytes.NewReader(data),
			})
			return nil
		}()
		if err != nil {
			return nil, err
		}
	}

	// wait for all blob uploading finished
	orasLayers, err := blobPusher.wait()
	if err != nil {
		return nil, err
	}
	logf("finished paralled pushing %d data layer", len(orasLayers))

	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("cannot flush repack zip file: %v", err)
	}
	if _, err := repackZip.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("cannot rewind repack zip file: %v", err)
	}
	fileStat, err := repackZip.Stat()
	if err != nil {
		return nil, fmt.Errorf("cannot calculate repack zip file stat: %v", err)
	}
	zr, err := zip.NewReader(repackZip, fileStat.Size())
	if err != nil {
		return nil, fmt.Errorf("cannot create repack file reader: %v", err)
	}

	logf("total repack zip file: %d", fileStat.Size())
	// update checkedModule
	m.blobr = repackZip
	m.size = fileStat.Size()
	m.zipr = zr

	return orasLayers, nil
}

func (p *anduinPatch) mergeManifestAnnotations(annotations map[string]string) map[string]string {
	annotationsFile := resolveModuleAnnotationFile()
	f, err := os.Open(annotationsFile)
	if err != nil && !os.IsNotExist(err) {
		logf("warn: unable to open manifest annotations file. File: `%s`. Err: %v", annotationsFile, err)
		return annotations
	}
	defer f.Close()
	logf("reading manifest annotations file: %s", annotationsFile)

	r := json.NewDecoder(f)
	var content struct {
		Manifest map[string]string `json:"$manifest"`
	}
	if err := r.Decode(&content); err != nil {
		logf("warn: unable to parse manifest annotations file. Err :%v", err)
		return annotations
	}

	// merging values
	if annotations == nil {
		annotations = map[string]string{}
	}
	for key, val := range content.Manifest {
		annotations[key] = val
	}
	return annotations
}

func shouldRepackFile(zf *zip.File) bool {
	return strings.HasSuffix(zf.Name, ".cue") ||
		strings.HasSuffix(zf.Name, ".json") ||
		strings.HasSuffix(zf.Name, ".yaml") ||
		strings.HasSuffix(zf.Name, ".yml") ||
		strings.ToLower(zf.Name) == "license"
}

func addFileToRepack(zr *zip.Writer, name string, r io.Reader) error {
	w, err := zr.Create(name)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, r)
	return err
}

func zipContentLayer(m *ocispec.Manifest) ocispec.Descriptor {
	layerLen := len(m.Layers)
	return m.Layers[layerLen-2]
}

func moduleFileLayer(m *ocispec.Manifest) ocispec.Descriptor {
	layerLen := len(m.Layers)
	return m.Layers[layerLen-1]
}

func validateModVersion(mv module.Version, mf *modfile.File) (string, bool) {
	major := mf.MajorVersion()
	if mv.Version() == "" || mv.Version() == "latest" {
		return major, true
	}

	// original validation logic
	wantMajor := semver.Major(mv.Version())
	if major != wantMajor {
		return major, false
	}

	return major, true
}

func resolveModuleAnnotationFile() string {
	envFile := os.Getenv(cueModuleAnnotationFileEnv)
	if envFile != "" {
		return envFile
	}
	// default to cwd `annotations.json`
	return "annotations.json"
}

func logf(f string, a ...any) {
	if logging {
		log.Printf("anduin: "+f, a...)
	}
}
