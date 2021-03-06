/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package mungers

import (
	"fmt"
	"regexp"
	"time"

	"k8s.io/test-infra/mungegithub/features"
	"k8s.io/test-infra/mungegithub/github"
	"k8s.io/test-infra/mungegithub/mungers/mungerutil"

	githubapi "github.com/google/go-github/github"
	"github.com/spf13/cobra"
)

const (
	day            = time.Hour * 24
	keepOpenLabel  = "keep-open"
	kindFlakeLabel = "kind/flake"
	stalePeriod    = 90 * day // Close the PR/Issue if no human interaction for `stalePeriod`
	startWarning   = 60 * day
	remindWarning  = 30 * day
	closingComment = `This %s hasn't been active in %s. Closing this %s. Please reopen if you would like to work towards merging this change, if/when the %s is ready for the next round of review.

%s
You can add 'keep-open' label to prevent this from happening again, or add a comment to keep it open another 90 days`
	warningComment = `This %s hasn't been active in %s. It will be closed in %s (%s).

%s
You can add 'keep-open' label to prevent this from happening, or add a comment to keep it open another 90 days`
)

var (
	closingCommentRE = regexp.MustCompile(`This \w hasn't been active in \d+ days?\..*label to prevent this from happening again`)
	warningCommentRE = regexp.MustCompile(`This \w hasn't been active in \d+ days?\..*be closed in \d+ days?`)
)

// CloseStale will ask the Bot to close any PR/Issue that didn't
// have any human interactions in `stalePeriod` duration.
//
// This is done by checking both review and issue comments, and by
// ignoring comments done with a bot name. We also consider re-open on the PR/Issue.
type CloseStale struct{}

func init() {
	s := CloseStale{}
	RegisterMungerOrDie(s)
	RegisterStaleComments(s)
}

// Name is the name usable in --pr-mungers
func (CloseStale) Name() string { return "close-stale" }

// RequiredFeatures is a slice of 'features' that must be provided
func (CloseStale) RequiredFeatures() []string { return []string{} }

// Initialize will initialize the munger
func (CloseStale) Initialize(config *github.Config, features *features.Features) error {
	return nil
}

// EachLoop is called at the start of every munge loop
func (CloseStale) EachLoop() error { return nil }

// AddFlags will add any request flags to the cobra `cmd`
func (CloseStale) AddFlags(cmd *cobra.Command, config *github.Config) {}

func findLastHumanPullRequestUpdate(obj *github.MungeObject) (*time.Time, bool) {
	pr, ok := obj.GetPR()
	if !ok {
		return nil, ok
	}

	comments, ok := obj.ListReviewComments()
	if !ok {
		return nil, ok
	}

	lastHuman := pr.CreatedAt
	for i := range comments {
		comment := comments[i]
		if comment.User == nil || comment.User.Login == nil || comment.CreatedAt == nil || comment.Body == nil {
			continue
		}
		if *comment.User.Login == botName || *comment.User.Login == jenkinsBotName {
			continue
		}
		if lastHuman.Before(*comment.UpdatedAt) {
			lastHuman = comment.UpdatedAt
		}
	}

	return lastHuman, true
}

func findLastHumanIssueUpdate(obj *github.MungeObject) (*time.Time, bool) {
	lastHuman := obj.Issue.CreatedAt

	comments, ok := obj.ListComments()
	if !ok {
		return nil, ok
	}

	for i := range comments {
		comment := comments[i]
		if !validComment(comment) {
			continue
		}
		if mergeBotComment(comment) || jenkinsBotComment(comment) {
			continue
		}
		if lastHuman.Before(*comment.UpdatedAt) {
			lastHuman = comment.UpdatedAt
		}
	}

	return lastHuman, true
}

func findLastInterestingEventUpdate(obj *github.MungeObject) (*time.Time, bool) {
	lastInteresting := obj.Issue.CreatedAt

	events, ok := obj.GetEvents()
	if !ok {
		return nil, ok
	}

	for i := range events {
		event := events[i]
		if event.Event == nil || *event.Event != "reopened" {
			continue
		}

		if lastInteresting.Before(*event.CreatedAt) {
			lastInteresting = event.CreatedAt
		}
	}

	return lastInteresting, true
}

func findLastModificationTime(obj *github.MungeObject) (*time.Time, bool) {
	lastHumanIssue, ok := findLastHumanIssueUpdate(obj)
	if !ok {
		return nil, ok
	}

	lastInterestingEvent, ok := findLastInterestingEventUpdate(obj)
	if !ok {
		return nil, ok
	}

	var lastModif *time.Time
	lastModif = lastHumanIssue

	if lastInterestingEvent.After(*lastModif) {
		lastModif = lastInterestingEvent
	}

	if obj.IsPR() {
		lastHumanPR, ok := findLastHumanPullRequestUpdate(obj)
		if !ok {
			return lastModif, true
		}

		if lastHumanPR.After(*lastModif) {
			lastModif = lastHumanPR
		}
	}

	return lastModif, true
}

// Find the last warning comment that the bot has posted.
// It can return an empty comment if it fails to find one, even if there are no errors.
func findLatestWarningComment(obj *github.MungeObject) (*githubapi.IssueComment, bool) {
	var lastFoundComment *githubapi.IssueComment

	comments, ok := obj.ListComments()
	if !ok {
		return nil, ok
	}

	for i := range comments {
		comment := comments[i]
		if !validComment(comment) {
			continue
		}
		if !mergeBotComment(comment) {
			continue
		}

		if !warningCommentRE.MatchString(*comment.Body) {
			continue
		}

		if lastFoundComment == nil || lastFoundComment.CreatedAt.Before(*comment.UpdatedAt) {
			if lastFoundComment != nil {
				obj.DeleteComment(lastFoundComment)
			}
			lastFoundComment = comment
		}
	}

	return lastFoundComment, true
}

func durationToDays(duration time.Duration) string {
	days := duration / day
	dayString := "days"
	if days == 1 || days == -1 {
		dayString = "day"
	}
	return fmt.Sprintf("%d %s", days, dayString)
}

func closeObj(obj *github.MungeObject, inactiveFor time.Duration) {
	mention := mungerutil.GetIssueUsers(obj.Issue).AllUsers().Mention().Join()
	if mention != "" {
		mention = "cc " + mention + "\n"
	}

	comment, ok := findLatestWarningComment(obj)
	if !ok {
		return
	}
	if comment != nil {
		obj.DeleteComment(comment)
	}

	var objType string

	if obj.IsPR() {
		objType = "PR"
	} else {
		objType = "Issue"
	}

	obj.WriteComment(fmt.Sprintf(closingComment, objType, durationToDays(inactiveFor), objType, objType, mention))

	if obj.IsPR() {
		obj.ClosePR()
	} else {
		obj.CloseIssuef("")
	}
}

func postWarningComment(obj *github.MungeObject, inactiveFor time.Duration, closeIn time.Duration) {
	mention := mungerutil.GetIssueUsers(obj.Issue).AllUsers().Mention().Join()
	if mention != "" {
		mention = "cc " + mention + "\n"
	}

	closeDate := time.Now().Add(closeIn).Format("Jan 2, 2006")

	var objType string

	if obj.IsPR() {
		objType = "PR"
	} else {
		objType = "Issue"
	}

	obj.WriteComment(fmt.Sprintf(
		warningComment,
		objType,
		durationToDays(inactiveFor),
		durationToDays(closeIn),
		closeDate,
		mention,
	))
}

func checkAndWarn(obj *github.MungeObject, inactiveFor time.Duration, closeIn time.Duration) {
	if closeIn < day {
		// We are going to close the PR/Issue in less than a day. Too late to warn
		return
	}
	comment, ok := findLatestWarningComment(obj)
	if !ok {
		return
	}
	if comment == nil {
		// We don't already have the comment. Post it
		postWarningComment(obj, inactiveFor, closeIn)
	} else if time.Since(*comment.UpdatedAt) > remindWarning {
		// It's time to warn again
		obj.DeleteComment(comment)
		postWarningComment(obj, inactiveFor, closeIn)
	} else {
		// We already have a warning, and it's not expired. Do nothing
	}
}

// Munge is the workhorse that will actually close the PRs/Issues
func (CloseStale) Munge(obj *github.MungeObject) {
	if !obj.IsPR() && !obj.HasLabel(kindFlakeLabel) {
		return
	}

	if obj.HasLabel(keepOpenLabel) {
		return
	}

	lastModif, ok := findLastModificationTime(obj)
	if !ok {
		return
	}

	closeIn := -time.Since(lastModif.Add(stalePeriod))
	inactiveFor := time.Since(*lastModif)
	if closeIn <= 0 {
		closeObj(obj, inactiveFor)
	} else if closeIn <= startWarning {
		checkAndWarn(obj, inactiveFor, closeIn)
	} else {
		// PR/Issue is active. Remove previous potential warning
		comment, ok := findLatestWarningComment(obj)
		if comment != nil && ok {
			obj.DeleteComment(comment)
		}
	}
}

func (CloseStale) isStaleComment(obj *github.MungeObject, comment *githubapi.IssueComment) bool {
	if !mergeBotComment(comment) {
		return false
	}

	if !closingCommentRE.MatchString(*comment.Body) {
		return false
	}

	return true
}

// StaleComments returns a slice of stale comments
func (s CloseStale) StaleComments(obj *github.MungeObject, comments []*githubapi.IssueComment) []*githubapi.IssueComment {
	return forEachCommentTest(obj, comments, s.isStaleComment)
}
