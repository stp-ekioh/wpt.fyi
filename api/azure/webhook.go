// Copyright 2018 The WPT Dashboard Project. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package azure

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"regexp"

	mapset "github.com/deckarep/golang-set"

	uc "github.com/web-platform-tests/wpt.fyi/api/receiver/client"
	"github.com/web-platform-tests/wpt.fyi/shared"
)

const uploaderName = "azure"

// Labels for runs from Azure Pipelines are determined from the artifact names.
// For master runs, artifact name may be either just "results" or something
// like "safari-results".
var (
	masterRegex        = regexp.MustCompile(`\bresults$`)
	prHeadRegex        = regexp.MustCompile(`\baffected-tests$`)
	prBaseRegex        = regexp.MustCompile(`\baffected-tests-without-changes$`)
	epochBranchesRegex = regexp.MustCompile("^refs/heads/epochs/.*")
)

func processBuild(
	aeAPI shared.AppEngineAPI,
	azureAPI API,
	owner,
	repo,
	sender,
	artifactName string,
	buildID int64,
) (bool, error) {
	build, err := azureAPI.GetBuild(owner, repo, buildID)
	if err != nil {
		return false, err
	}
	if build == nil {
		return false, fmt.Errorf("cannot get build %s/%s/%d", owner, repo, buildID)
	}
	sha := build.TriggerInfo.SourceSHA

	// https://docs.microsoft.com/en-us/rest/api/azure/devops/build/artifacts/get?view=azure-devops-rest-4.1
	artifactsURL := azureAPI.GetAzureArtifactsURL(owner, repo, buildID)

	log := shared.GetLogger(aeAPI.Context())
	log.Infof("Fetching %s", artifactsURL)

	client := aeAPI.GetHTTPClient()
	req, err := http.NewRequestWithContext(aeAPI.Context(), http.MethodGet, artifactsURL, nil)
	if err != nil {
		return false, fmt.Errorf("failed to create get for %s/%s/%d: %w", owner, repo, buildID, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, fmt.Errorf("failed to fetch artifacts for %s/%s/%d: %w", owner, repo, buildID, err)
	}

	var artifacts BuildArtifacts
	if body, err := ioutil.ReadAll(resp.Body); err != nil {
		return false, fmt.Errorf("failed to read response body: %w", err)
	} else if err = json.Unmarshal(body, &artifacts); err != nil {
		return false, fmt.Errorf("failed to unmarshal JSON: %w", err)
	}

	uploadedAny := false
	errors := make(chan (error), artifacts.Count)
	for _, artifact := range artifacts.Value {
		if artifactName != "" && artifactName != artifact.Name {
			log.Infof("Skipping artifact %s (looking for %s)", artifact.Name, artifactName)

			continue
		}
		log.Infof("Uploading %s for %s/%s build %v...", artifact.Name, owner, repo, buildID)

		labels := mapset.NewSet()
		if sender != "" {
			labels.Add(shared.GetUserLabel(sender))
		}

		if masterRegex.MatchString(artifact.Name) {
			if build.IsMasterBranch() || epochBranchesRegex.MatchString(build.SourceBranch) {
				labels.Add(shared.MasterLabel)
			}
		} else if prHeadRegex.MatchString(artifact.Name) {
			labels.Add(shared.PRHeadLabel)
		} else if prBaseRegex.MatchString(artifact.Name) {
			labels.Add(shared.PRBaseLabel)
		}

		uploader, err := aeAPI.GetUploader(uploaderName)
		if err != nil {
			return false, fmt.Errorf("failed to get uploader creds from Datastore: %w", err)
		}

		uploadClient := uc.NewClient(aeAPI)
		err = uploadClient.CreateRun(
			sha,
			uploader.Username,
			uploader.Password,
			// Azure has a single zip artifact, special-cased by the receiver.
			[]string{artifact.Resource.DownloadURL},
			nil,
			shared.ToStringSlice(labels))
		if err != nil {
			errors <- fmt.Errorf("failed to create run: %w", err)
		} else {
			uploadedAny = true
		}
	}
	close(errors)
	for err := range errors {
		return uploadedAny, err
	}

	return uploadedAny, nil
}
