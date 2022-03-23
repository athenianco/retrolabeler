package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/gobwas/glob"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/schollz/progressbar/v3"
	"github.com/shurcooL/githubv4"
	"golang.org/x/oauth2"
	"gopkg.in/yaml.v3"
)

type Label struct {
	Name string
	All  []glob.Glob
	Any  []glob.Glob
}

type matchObject struct {
	Any []string `yaml:"omitempty"`
	All []string `yaml:"omitempty"`
}

func parseGlob(val string, label string) (globby glob.Glob, err error) {
	if globby, err = glob.Compile(val); err != nil {
		log.Error().Msgf("Failed to parse glob \"%v\" for label %v, skipped", val, label)
	}
	return
}

func parseGlobArray(vals []string, label string) (globs []glob.Glob, err error) {
	for _, str := range vals {
		var globby glob.Glob
		if globby, err = parseGlob(str, label); err != nil {
			return
		}
		globs = append(globs, globby)
	}
	return
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
		var globby glob.Glob
		var str string
		if err = node.Decode(&str); err == nil {
			if globby, err = parseGlob(str, label); err == nil {
				labels = append(labels, Label{Name: label, Any: []glob.Glob{globby}})
			}
			continue
		}
		var arr []string
		if err = node.Decode(&arr); err == nil {
			if anyGlobs, err := parseGlobArray(arr, label); err == nil {
				labels = append(labels, Label{Name: label, Any: anyGlobs})
			}
			continue
		}
		var obj matchObject
		if err = node.Decode(&obj); err != nil {
			log.Error().Msgf("Failed to parse label %v, skipped", label)
			continue
		}
		var anyGlobs, allGlobs []glob.Glob
		if anyGlobs, err = parseGlobArray(obj.Any, label); err != nil {
			continue
		}
		if allGlobs, err = parseGlobArray(obj.All, label); err != nil {
			continue
		}
		if len(anyGlobs) == 0 && len(allGlobs) == 0 {
			continue
		}
		labels = append(labels, Label{Name: label, Any: anyGlobs, All: allGlobs})
	}
	return labels, err
}

func makeGraphQLClient(token string) *githubv4.Client {
	httpClient := oauth2.NewClient(context.Background(), oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: token},
	))
	return githubv4.NewClient(httpClient)
}

func LoadLabels(repo, token string) (map[string]string, error) {
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
			log.Error().Msgf("Failed to fetch pull requests from GitHub: %v", err)
			return nil, err
		}
		for _, node := range query.Repository.Labels.Nodes {
			labelMap[node.Name] = node.Id
		}
		if !query.Repository.Labels.PageInfo.HasNextPage {
			break
		}
		variables["cursor"] = githubv4.NewString(query.Repository.Labels.PageInfo.EndCursor)
	}
	return labelMap, nil
}

func CheckLabels(labels []Label, labelMap map[string]string) bool {
	for _, label := range labels {
		if _, exists := labelMap[label.Name]; !exists {
			log.Error().Msgf("Label %v does not exist in the repository", label.Name)
			return false
		}
	}
	return true
}

type PullRequest struct {
	Id     string
	Paths  []string
	Labels map[string]struct{}
}

func LoadPullRequests(repo, token string) ([]PullRequest, error) {
	client := makeGraphQLClient(token)
	var query struct {
		Search struct {
			PageInfo struct {
				HasNextPage bool
				EndCursor   githubv4.String
			}
			Nodes []struct {
				PullRequest struct {
					Id    string
					Files struct {
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
	variables := map[string]interface{}{
		"query": githubv4.String(fmt.Sprintf("repo:%s is:pr created:>%s",
			repo, time.Now().AddDate(-1, -1, 0).Format("2006-01-02"))),
		"cursor": (*githubv4.String)(nil),
	}
	var prs []PullRequest
	for {
		err := client.Query(context.Background(), &query, variables)
		if err != nil {
			log.Error().Msgf("Failed to fetch pull requests from GitHub: %v", err)
			return nil, err
		}
		for _, node := range query.Search.Nodes {
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
		if !query.Search.PageInfo.HasNextPage {
			break
		}
		variables["cursor"] = githubv4.NewString(query.Search.PageInfo.EndCursor)
	}
	return prs, nil
}

type Update struct {
	Id     string
	Labels []string
}

func ComputeUpdates(prs []PullRequest, rules []Label, labelMap map[string]string) []Update {
	var updates []Update
	bar := progressbar.Default(int64(len(prs)))
	for _, pr := range prs {
		var newLabels []string
		for _, rule := range rules {
			if _, exists := pr.Labels[rule.Name]; exists {
				continue
			}
			passed := len(rule.Any) == 0
			for _, anyGlob := range rule.Any {
				for _, path := range pr.Paths {
					if anyGlob.Match(path) {
						passed = true
						break
					}
				}
				if passed {
					break
				}
			}
			for _, allGlob := range rule.All {
				allMatched := false
				for _, path := range pr.Paths {
					if allGlob.Match(path) {
						allMatched = true
						break
					}
				}
				if !allMatched {
					passed = false
					break
				}
			}
			if passed {
				newLabels = append(newLabels, labelMap[rule.Name])
			}
		}
		if len(newLabels) > 0 {
			updates = append(updates, Update{Id: pr.Id, Labels: newLabels})
		}
		_ = bar.Add(1)
	}
	return updates
}

func ApplyUpdates(updates []Update, token string) error {
	client := makeGraphQLClient(token)
	var mutation struct {
		AddLabelsToLabelable struct {
			ClientMutationID string
		} `graphql:"addLabelsToLabelable(input: $input)"`
	}
	bar := progressbar.Default(int64(len(updates)))
	for _, update := range updates {
		labelIds := make([]githubv4.ID, len(update.Labels))
		for i, label := range update.Labels {
			labelIds[i] = label
		}
		input := githubv4.AddLabelsToLabelableInput{
			LabelableID: update.Id,
			LabelIDs:    labelIds,
		}
		if err := client.Mutate(context.Background(), &mutation, input, nil); err != nil {
			log.Error().Msgf("Failed to label PR %v: %v", update.Id, err)
		}
		_ = bar.Add(1)
	}
	return nil
}

func main() {
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})
	log.Info().Msg("Initializing")
	_, err := os.Stdin.Stat()
	token := os.Getenv("GITHUB_TOKEN")
	if len(os.Args) != 2 || err != nil || token == "" {
		log.Error().Msg("Usage: cat labeler.yml | GITHUB_TOKEN=... retrolabeler your/repository")
		os.Exit(1)
	}
	log.Info().Msg("Reading labeler.yml from stdin")
	labels, err := ParseLabelerConfig()
	if err != nil {
		os.Exit(2)
	}
	log.Info().Msgf("Parsed %d labels", len(labels))
	repo := os.Args[1]
	log.Info().Msg("Resolving labels")
	labelMap, err := LoadLabels(repo, token)
	if err != nil {
		os.Exit(3)
	}
	log.Info().Msgf("Loaded %d labels", len(labelMap))
	if !CheckLabels(labels, labelMap) {
		os.Exit(4)
	}
	log.Info().Msgf("Discovering PRs in %v", repo)
	prs, err := LoadPullRequests(repo, token)
	if err != nil {
		os.Exit(5)
	}
	log.Info().Msgf("Loaded %d pull requests", len(prs))
	updates := ComputeUpdates(prs, labels, labelMap)
	log.Info().Msgf("Computed %d PR updates", len(updates))
	if len(updates) == 0 {
		return
	}
	log.Info().Msgf("Labeling the pull requests")
	err = ApplyUpdates(updates, token)
	if err != nil {
		os.Exit(6)
	}
}
