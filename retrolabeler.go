package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gobwas/glob"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/schollz/progressbar/v3"
	"github.com/shurcooL/githubv4"
	"golang.org/x/oauth2"
	"gopkg.in/yaml.v3"
)

type RulePackage struct {
	IncludeAll []glob.Glob
	IncludeAny []glob.Glob
	ExcludeAll []glob.Glob
	ExcludeAny []glob.Glob
}

type Label struct {
	Name     string
	Packages []RulePackage
}

type matchObject struct {
	Any []string `yaml:",omitempty"`
	All []string `yaml:",omitempty"`
}

func parseGlob(val string, label string) (globInclude glob.Glob, globExclude glob.Glob, err error) {
	var globby *glob.Glob
	if len(val) > 0 && val[0] == '!' {
		val = val[1:]
		globby = &globExclude
	} else {
		globby = &globInclude
	}
	if *globby, err = glob.Compile(val, '/'); err != nil {
		log.Error().Msgf("Failed to parse glob \"%v\" for label %v, skipped", val, label)
	}
	return
}

func parseGlobArray(vals []string, label string) (includeGlobs []glob.Glob, excludeGlobs []glob.Glob, err error) {
	for _, str := range vals {
		var globInclude, globExclude glob.Glob
		if globInclude, globExclude, err = parseGlob(str, label); err != nil {
			return
		}
		if globInclude != nil {
			includeGlobs = append(includeGlobs, globInclude)
		}
		if globExclude != nil {
			excludeGlobs = append(excludeGlobs, globExclude)
		}
	}
	return
}

func Initialize() (string, string, string, int, bool, bool, error) {
	var workers uint
	var createMissingLabels, dryRun bool
	var since string
	flag.UintVar(&workers, "j", 2, "Number of parallel workers to label PRs.")
	flag.BoolVar(&createMissingLabels, "c", false, "Create the missing labels.")
	flag.StringVar(&since, "s", time.Now().AddDate(-1, -3, 0).Format("2006-01-02"),
		"Search for PRs created after this date.")
	flag.BoolVar(&dryRun, "dry-run", false, "Execute all the steps except the actual labeling.")
	flag.Parse()
	_, err := os.Stdin.Stat()
	token := os.Getenv("GITHUB_TOKEN")
	if len(flag.Args()) != 1 || err != nil || token == "" {
		log.Error().Msg("Usage: cat labeler.yml | GITHUB_TOKEN=... retrolabeler your/repository")
		if err == nil {
			err = errors.New("len(os.Args) != 2 || token == \"\"")
		}
		return "", "", "", 0, false, false, err
	}
	if workers == 0 {
		log.Error().Msg("-j value must be positive")
		return "", "", "", 0, false, false, errors.New("-j value must be positive")
	}
	if dryRun {
		log.Warn().Msg("Dry run mode: will not label PRs")
	}
	return flag.Args()[0], since, token, int(workers), createMissingLabels, dryRun, nil
}

func ParseLabelerConfig() ([]Label, error) {
	var labelNodes map[string]yaml.Node
	var err error
	if err = yaml.NewDecoder(os.Stdin).Decode(&labelNodes); err != nil {
		log.Error().Msgf("Failed to read from stdin: %v", err)
		return nil, err
	}
	var labels []Label
	for label, node := range labelNodes {
		var str string
		if err = node.Decode(&str); err == nil {
			if globInclude, globExclude, err := parseGlob(str, label); err == nil {
				parsed := Label{Name: label, Packages: make([]RulePackage, 1)}
				if globInclude != nil {
					parsed.Packages[0].IncludeAny = append(parsed.Packages[0].IncludeAny, globInclude)
				}
				if globExclude != nil {
					parsed.Packages[0].ExcludeAny = append(parsed.Packages[0].ExcludeAny, globExclude)
				}
				labels = append(labels, parsed)
			}
			continue
		}
		var arr []string
		if err = node.Decode(&arr); err == nil {
			if includeGlobs, excludeGlobs, err := parseGlobArray(arr, label); err == nil {
				labels = append(labels, Label{
					Name:     label,
					Packages: []RulePackage{{IncludeAny: includeGlobs, ExcludeAny: excludeGlobs}},
				})
			}
			continue
		}
		var objs []matchObject
		if err = node.Decode(&objs); err != nil {
			log.Error().Msgf("Failed to parse label %v, skipped", label)
			continue
		}
		var packages []RulePackage
		for _, obj := range objs {
			var includeAnyGlobs, excludeAnyGlobs, includeAllGlobs, excludeAllGlobs []glob.Glob
			if includeAnyGlobs, excludeAnyGlobs, err = parseGlobArray(obj.Any, label); err != nil {
				continue
			}
			if includeAllGlobs, excludeAllGlobs, err = parseGlobArray(obj.All, label); err != nil {
				continue
			}
			if len(includeAnyGlobs) == 0 && len(excludeAnyGlobs) == 0 && len(includeAllGlobs) == 0 && len(excludeAllGlobs) == 0 {
				continue
			}
			packages = append(packages, RulePackage{
				IncludeAll: includeAllGlobs,
				IncludeAny: includeAnyGlobs,
				ExcludeAll: excludeAllGlobs,
				ExcludeAny: excludeAnyGlobs,
			})
		}
		labels = append(labels, Label{Name: label, Packages: packages})
	}
	return labels, err
}

type LabelPreviewWrapper struct {
	Transport http.RoundTripper
}

func (w LabelPreviewWrapper) RoundTrip(req *http.Request) (*http.Response, error) {
	req.Header.Set("Accept", "application/vnd.github.bane-preview+json")
	return w.Transport.RoundTrip(req)
}

func makeGraphQLClient(token string) *githubv4.Client {
	httpClient := oauth2.NewClient(context.Background(), oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: token},
	))
	httpClient.Transport = LabelPreviewWrapper{httpClient.Transport}
	return githubv4.NewClient(httpClient)
}

func LoadLabels(repo, token string) (string, map[string]string, error) {
	client := makeGraphQLClient(token)
	var query struct {
		Repository struct {
			Id     string
			Labels struct {
				PageInfo struct {
					HasNextPage bool
					EndCursor   githubv4.String
				}
				Nodes []struct {
					Id   string
					Name string
				}
			} `graphql:"labels(first: 100, after: $cursor)"`
		} `graphql:"repository(name: $name, owner: $owner)"`
	}
	variables := map[string]interface{}{
		"name":   githubv4.String(strings.Split(repo, "/")[1]),
		"owner":  githubv4.String(strings.Split(repo, "/")[0]),
		"cursor": (*githubv4.String)(nil),
	}
	labelMap := map[string]string{}
	for {
		err := client.Query(context.Background(), &query, variables)
		if err != nil {
			log.Error().Msgf("Failed to fetch labels from GitHub: %v", err)
			return "", nil, err
		}
		for _, node := range query.Repository.Labels.Nodes {
			labelMap[node.Name] = node.Id
		}
		if !query.Repository.Labels.PageInfo.HasNextPage {
			break
		}
		variables["cursor"] = githubv4.NewString(query.Repository.Labels.PageInfo.EndCursor)
	}
	return query.Repository.Id, labelMap, nil
}

func CheckLabels(labels []Label, labelMap map[string]string, createMissingLabels bool) []string {
	var missingLabels []string
	for _, label := range labels {
		if _, exists := labelMap[label.Name]; !exists {
			missingLabels = append(missingLabels, label.Name)
		}
	}
	if len(missingLabels) > 0 {
		var event *zerolog.Event
		if createMissingLabels {
			event = log.Warn()
		} else {
			event = log.Error()
		}
		event.Msgf("%d labels do not exist in the repository: %v",
			len(missingLabels), strings.Join(missingLabels, ", "))
	}
	return missingLabels
}

type CreateLabelInput struct {
	Name         githubv4.String `json:"name"`
	Color        githubv4.String `json:"color"`
	RepositoryId githubv4.ID     `json:"repositoryId"`
}

func CreateLabels(repoId string, labels []string, labelMap map[string]string, token string) error {
	log.Info().Msgf("Creating %d labels", len(labels))
	bar := progressbar.Default(int64(len(labels)))
	defer bar.Finish()
	client := makeGraphQLClient(token)
	var mutation struct {
		CreateLabel struct {
			ClientMutationID string
			Label            struct {
				Id string
			}
		} `graphql:"createLabel(input: $input)"`
	}
	var failed bool
	for _, label := range labels {
		input := CreateLabelInput{
			Name:         githubv4.String(label),
			Color:        githubv4.String("cccccc"),
			RepositoryId: githubv4.ID(repoId),
		}
		if err := client.Mutate(context.Background(), &mutation, input, nil); err != nil {
			log.Error().Msgf("Failed to create label %v: %v", label, err)
			failed = true
		}
		labelMap[label] = mutation.CreateLabel.Label.Id
		_ = bar.Add(1)
	}
	if failed {
		return errors.New("failed to create missing labels")
	}
	return nil
}

type PullRequest struct {
	Id     string
	Paths  []string
	Labels map[string]struct{}
}

func LoadPullRequests(repo, since, token string) ([]PullRequest, error) {
	var bar *progressbar.ProgressBar
	client := makeGraphQLClient(token)
	var query struct {
		RateLimit struct {
			Cost      int
			Remaining int
			ResetAt   githubv4.DateTime
		}
		Search struct {
			IssueCount int
			PageInfo   struct {
				HasNextPage bool
				EndCursor   githubv4.String
			}
			Nodes []struct {
				PullRequest struct {
					Id        string
					CreatedAt githubv4.DateTime
					Files     struct {
						Nodes []struct {
							Path string
						}
					} `graphql:"files(first: 100)"`
					Labels struct {
						Nodes []struct {
							Name string
						}
					} `graphql:"labels(first: 100)"`
				} `graphql:"... on PullRequest"`
			}
		} `graphql:"search(first: 100, after: $cursor, query: $query, type: ISSUE)"`
	}
	createdUntil := time.Now().AddDate(0, 0, 1).Format("2006-01-02")
	setVariables := func() map[string]interface{} {
		return map[string]interface{}{
			"query": githubv4.String(fmt.Sprintf("repo:%s is:pr created:%s..%s sort:created-desc",
				repo, since, createdUntil)),
			"cursor": (*githubv4.String)(nil),
		}
	}
	variables := setVariables()
	var prs []PullRequest
	fetchedIds := map[string]struct{}{}
	attempts := 10
	for {
		var err error
		for attempt := 1; attempt <= attempts; attempt++ {
			err = client.Query(context.Background(), &query, variables)
			if err != nil {
				log.Error().Msgf("[%d/%d] Failed to fetch pull requests from GitHub: %v",
					attempt, attempts, err)
			} else {
				break
			}
		}
		if err != nil {
			return nil, err
		}
		if bar == nil {
			bar = progressbar.Default(int64(query.Search.IssueCount))
			defer bar.Finish()
		}
		_ = bar.Add(len(query.Search.Nodes))
		hasNew := false
		for _, node := range query.Search.Nodes {
			if _, exists := fetchedIds[node.PullRequest.Id]; exists {
				_ = bar.Add(-1)
				continue
			}
			hasNew = true
			fetchedIds[node.PullRequest.Id] = struct{}{}
			var paths []string
			labels := map[string]struct{}{}
			for _, file := range node.PullRequest.Files.Nodes {
				paths = append(paths, file.Path)
			}
			for _, label := range node.PullRequest.Labels.Nodes {
				labels[label.Name] = struct{}{}
			}
			prs = append(prs, PullRequest{Id: node.PullRequest.Id, Paths: paths, Labels: labels})
		}
		if !hasNew {
			break
		}
		createdUntil = query.Search.Nodes[len(query.Search.Nodes)-1].PullRequest.CreatedAt.Format("2006-01-02")
		bar.Describe(fmt.Sprintf("✔ since %v [%d]", createdUntil, query.RateLimit.Remaining))
		if query.RateLimit.Remaining < query.RateLimit.Cost*10 {
			log.Warn().Msgf("Approached the rate limit, sleeping until %v",
				query.RateLimit.ResetAt.Format(time.RFC3339))
			time.Sleep(time.Until(query.RateLimit.ResetAt.Time))
		}
		if !query.Search.PageInfo.HasNextPage {
			variables = setVariables()
		} else {
			variables["cursor"] = githubv4.NewString(query.Search.PageInfo.EndCursor)
		}
	}
	return prs, nil
}

type Update struct {
	Id     string
	Labels []string
}

func ComputeUpdates(prs []PullRequest, labels []Label, labelMap map[string]string) []Update {
	var updates []Update
	bar := progressbar.Default(int64(len(prs)))
	defer bar.Finish()
	for _, pr := range prs {
		var newLabels []string
		for _, label := range labels {
			if _, exists := pr.Labels[label.Name]; exists {
				continue
			}
			var passed bool
			for _, pkg := range label.Packages {
				for _, path := range pr.Paths {
					passed = true
					for _, includeGlob := range pkg.IncludeAny {
						if !includeGlob.Match(path) {
							passed = false
							break
						}
					}
					if !passed {
						continue
					}
					for _, excludeGlob := range pkg.ExcludeAny {
						if excludeGlob.Match(path) {
							passed = false
							break
						}
					}
					if passed {
						break
					}
				}
				if !passed {
					continue
				}
				for _, includeGlob := range pkg.IncludeAll {
					for _, path := range pr.Paths {
						if !includeGlob.Match(path) {
							passed = false
							break
						}
					}
					if !passed {
						break
					}
				}
				if !passed {
					continue
				}
				for _, excludeGlob := range pkg.ExcludeAll {
					for _, path := range pr.Paths {
						if excludeGlob.Match(path) {
							passed = false
							break
						}
					}
				}
				if passed {
					break
				}
			}
			if passed {
				newLabels = append(newLabels, labelMap[label.Name])
			}
		}
		if len(newLabels) > 0 {
			updates = append(updates, Update{Id: pr.Id, Labels: newLabels})
		}
		_ = bar.Add(1)
	}
	return updates
}

func ApplyUpdates(updates []Update, token string, workers int, dryRun bool) error {
	bar := progressbar.Default(int64(len(updates)))
	defer bar.Finish()
	tasks := make(chan Update, len(updates))
	for _, update := range updates {
		tasks <- update
	}
	close(tasks)
	var rateLimitRecoverLock sync.Mutex
	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()
		client := makeGraphQLClient(token)
		var query struct {
			RateLimit struct {
				Cost      int
				Remaining int
				ResetAt   githubv4.DateTime
			}
		}
		for len(tasks) > 0 {
			if err := client.Query(context.Background(), &query, nil); err == nil {
				bar.Describe(fmt.Sprintf("[%d]", query.RateLimit.Remaining))
				if query.RateLimit.Remaining < 50 {
					log.Warn().Msgf("Approached the rate limit, sleeping until %v",
						query.RateLimit.ResetAt.Format(time.RFC3339))
					rateLimitRecoverLock.Lock()
					time.Sleep(time.Until(query.RateLimit.ResetAt.Time))
					rateLimitRecoverLock.Unlock()
				}
			}
		}
	}()

	var secondaryTasks []Update
	var secondaryLock sync.Mutex
	attempts := 10
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			client := makeGraphQLClient(token)
			var mutation struct {
				AddLabelsToLabelable struct {
					ClientMutationID string
				} `graphql:"addLabelsToLabelable(input: $input)"`
			}
			var dryRunQuery struct {
				Node struct {
					Id string
				} `graphql:"node(id: $node)"`
			}
			for update := range tasks {
				labelIds := make([]githubv4.ID, len(update.Labels))
				for i, label := range update.Labels {
					labelIds[i] = label
				}
				input := githubv4.AddLabelsToLabelableInput{
					LabelableID: update.Id,
					LabelIDs:    labelIds,
				}
				var err error
				if dryRun {
					variables := map[string]interface{}{
						"node": githubv4.ID(update.Id),
					}
					err = client.Query(context.Background(), &dryRunQuery, variables)
				} else {
					for attempt := 1; attempt <= attempts; attempt++ {
						err = client.Mutate(context.Background(), &mutation, input, nil)
						if err != nil {
							log.Error().Msgf("[%d/%d] Failed to label pull request %v: %v",
								attempt, attempts, update.Id, err)
						} else {
							break
						}
					}
				}
				if err != nil {
					if strings.Contains(err.Error(), "secondary rate limit") {
						secondaryLock.Lock()
						secondaryTasks = append(secondaryTasks, update)
						secondaryLock.Unlock()
						log.Warn().Msg("Sleeping 60s on the secondary rate limit, try a smaller number of workers (-j)")
						time.Sleep(time.Minute)
					} else {
						log.Error().Msgf("Failed to label %v: %v", update.Id, err)
					}
				}
				_ = bar.Add(1)
				rateLimitRecoverLock.Lock()
				//lint:ignore SA2001 must wait until the rate limit restores
				rateLimitRecoverLock.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(secondaryTasks) > 0 {
		log.Info().Msg("Repeating the rate-limited operations")
		return ApplyUpdates(secondaryTasks, token, workers, dryRun)
	}
	return nil
}

func main() {
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})
	log.Info().Msg("Initializing")
	repo, since, token, workers, createMissingLabels, dryRun, err := Initialize()
	if err != nil {
		os.Exit(1)
	}
	log.Info().Msg("Reading labeler.yml from stdin")
	labels, err := ParseLabelerConfig()
	if err != nil {
		os.Exit(2)
	}
	log.Info().Msgf("Parsed %d labels", len(labels))
	log.Info().Msg("Resolving labels")
	repoId, labelMap, err := LoadLabels(repo, token)
	if err != nil {
		os.Exit(3)
	}
	log.Info().Msgf("Loaded %d labels", len(labelMap))
	missingLabels := CheckLabels(labels, labelMap, createMissingLabels || dryRun)
	if len(missingLabels) > 0 {
		if dryRun {
			for _, label := range missingLabels {
				labelMap[label] = label
			}
		} else if !createMissingLabels || CreateLabels(repoId, missingLabels, labelMap, token) != nil {
			os.Exit(4)
		}
	}
	log.Info().Msgf("Discovering PRs in %v", repo)
	prs, err := LoadPullRequests(repo, since, token)
	if err != nil {
		os.Exit(5)
	}
	log.Info().Msgf("Loaded %d pull requests", len(prs))
	updates := ComputeUpdates(prs, labels, labelMap)
	log.Info().Msgf("Computed %d PR updates", len(updates))
	if len(updates) == 0 {
		return
	}
	log.Info().Msgf("Labeling the pull requests in %d parallel workers", workers)
	err = ApplyUpdates(updates, token, workers, dryRun)
	if err != nil {
		os.Exit(6)
	}
}
