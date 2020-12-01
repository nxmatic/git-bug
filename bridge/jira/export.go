package jira

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/pkg/errors"

	"github.com/MichaelMure/git-bug/bridge/core"
	"github.com/MichaelMure/git-bug/bridge/core/auth"
	"github.com/MichaelMure/git-bug/bug"
	"github.com/MichaelMure/git-bug/cache"
	"github.com/MichaelMure/git-bug/entity"
	"github.com/MichaelMure/git-bug/identity"
)

var (
	ErrMissingCredentials = errors.New("missing credentials")
)

// jiraExporter implement the Exporter interface
type jiraExporter struct {
	name         string
	labelsJiraID string
	conf         core.Configuration

	// cache identities clients
	identityClient map[entity.Id]*Client

	// the mapping from git-bug "status" to JIRA "status" id
	statusMap map[string]string

	// cache identifiers used to speed up exporting operations
	// cleared for each bug
	cachedOperationIDs map[entity.Id]string

	// cache labels used to speed up exporting labels events
	cachedLabels map[string]string

	// store JIRA project information
	project *Project
}

// Init .
func (je *jiraExporter) Init(ctx context.Context, repo *cache.RepoCache, name string, conf core.Configuration) error {
	je.name = name
	je.conf = conf
	je.labelsJiraID = "labels"
	je.identityClient = make(map[entity.Id]*Client)
	je.cachedOperationIDs = make(map[entity.Id]string)
	je.cachedLabels = make(map[string]string)

	statusMap, err := getStatusMap(je.conf)
	if err != nil {
		return err
	}
	je.statusMap = statusMap

	if je.conf[confKeyLabelsId] != "" {
		je.labelsJiraID = je.conf[confKeyLabelsId]
	}
	// preload all clients
	err = je.cacheAllClient(ctx, repo)
	if err != nil {
		return err
	}

	if len(je.identityClient) == 0 {
		return fmt.Errorf("no credentials for this bridge")
	}

	var client *Client
	for _, c := range je.identityClient {
		client = c
		break
	}

	if client == nil {
		panic("nil client")
	}

	je.project, err = client.GetProject(je.conf[confKeyProject])
	if err != nil {
		return err
	}

	return nil
}

func (je *jiraExporter) cacheAllClient(ctx context.Context, repo *cache.RepoCache) error {
	creds, err := auth.List(repo,
		auth.WithTarget(target),
		auth.WithKind(auth.KindLoginPassword), auth.WithKind(auth.KindLogin),
		auth.WithMeta(auth.MetaKeyBaseURL, je.conf[confKeyBaseUrl]),
	)
	if err != nil {
		return err
	}

	for _, cred := range creds {
		login, ok := cred.GetMetadata(auth.MetaKeyLogin)
		if !ok {
			_, _ = fmt.Fprintf(os.Stderr, "credential %s is not tagged with a Jira login\n", cred.ID().Human())
			continue
		}

		user, err := repo.ResolveIdentityImmutableMetadata(metaKeyJiraLogin, login)
		if err == identity.ErrIdentityNotExist {
			continue
		}
		if err != nil {
			return nil
		}

		if _, ok := je.identityClient[user.Id()]; !ok {
			client, err := buildClient(ctx, je.conf[confKeyBaseUrl], je.conf[confKeyCredentialType], creds[0])
			if err != nil {
				return err
			}
			je.identityClient[user.Id()] = client
		}
	}

	return nil
}

// getClientForIdentity return an API client configured with the credentials
// of the given identity. If no client were found it will initialize it from
// the known credentials and cache it for next use.
func (je *jiraExporter) getClientForIdentity(userId entity.Id) (*Client, error) {
	client, ok := je.identityClient[userId]
	if ok {
		return client, nil
	}

	return nil, ErrMissingCredentials
}

// ExportAll export all event made by the current user to Jira
func (je *jiraExporter) ExportAll(ctx context.Context, repo *cache.RepoCache, since time.Time) (<-chan core.ExportResult, error) {
	out := make(chan core.ExportResult)

	go func() {
		defer close(out)

		var allIdentitiesIds []entity.Id
		for id := range je.identityClient {
			allIdentitiesIds = append(allIdentitiesIds, id)
		}

		allBugsIds := repo.AllBugsIds()

		for _, id := range allBugsIds {
			b, err := repo.ResolveBug(id)
			if err != nil {
				out <- core.NewExportError(errors.Wrap(err, "can't load bug"), id)
				return
			}

			select {

			case <-ctx.Done():
				// stop iterating if context cancel function is called
				return

			default:
				snapshot := b.Snapshot()

				// ignore issues whose last modification date is before the query date
				// TODO: compare the Lamport time instead of using the unix time
				if snapshot.CreateTime.Before(since) {
					out <- core.NewExportNothing(b.Id(), "bug created before the since date")
					continue
				}

				if snapshot.HasAnyActor(allIdentitiesIds...) {
					// try to export the bug and it associated events
					err := je.exportBug(ctx, b, out)
					if err != nil {
						out <- core.NewExportError(errors.Wrap(err, "can't export bug"), id)
						return
					}
				} else {
					out <- core.NewExportNothing(id, "not an actor")
				}
			}
		}
	}()

	return out, nil
}

// exportBug publish bugs and related events
func (je *jiraExporter) exportBug(ctx context.Context, b *cache.BugCache, out chan<- core.ExportResult) error {
	snapshot := b.Snapshot()

	var bugJiraID string

	// Special case:
	// if a user try to export a bug that is not already exported to jira (or
	// imported from jira) and we do not have the token of the bug author,
	// there is nothing we can do.

	// first operation is always createOp
	createOp := snapshot.Operations[0].(*bug.CreateOperation)
	author := snapshot.Author

	// skip bug if it was imported from some other bug system
	origin, ok := snapshot.GetCreateMetadata(core.MetaKeyOrigin)
	if ok && origin != target {
		out <- core.NewExportNothing(
			b.Id(), fmt.Sprintf("issue tagged with origin: %s", origin))
		return nil
	}

	// skip bug if it is a jira bug but is associated with another project
	// (one bridge per JIRA project)
	bridge, ok := snapshot.GetCreateMetadata(metaKeyJiraBridge)
	if ok && je.name != bridge {
		out <- core.NewExportNothing(
			b.Id(), fmt.Sprintf("issue tagged with project: %s", bridge))
		return nil
	}

	// get jira bug ID
	jiraID, ok := snapshot.GetCreateMetadata(metaKeyJiraId)
	if ok {
		// will be used to mark operation related to a bug as exported
		bugJiraID = jiraID
	} else {
		// check that we have credentials for operation author
		client, err := je.getClientForIdentity(author.Id())
		if err != nil {
			// if bug is not yet exported and we do not have the author's credentials
			// then there is nothing we can do, so just skip this bug
			out <- core.NewExportNothing(
				b.Id(), fmt.Sprintf("missing author credentials for user %.8s",
					author.Id().String()))
			return err
		}

		// Load any custom fields required to create an issue from the git
		// config file.
		fields := make(map[string]interface{})
		defaultFields, hasConf := je.conf[confKeyCreateDefaults]
		if hasConf {
			err = json.Unmarshal([]byte(defaultFields), &fields)
			if err != nil {
				return err
			}
		} else {
			// If there is no configuration provided, at the very least the
			// "issueType" field is always required. 10001 is "story" which I'm
			// pretty sure is standard/default on all JIRA instances.
			fields["issuetype"] = map[string]interface{}{
				"id": "10001",
			}
		}
		bugIDField, hasConf := je.conf[confKeyCreateGitBug]
		if hasConf {
			// If the git configuration also indicates it, we can assign the git-bug
			// id to a custom field to assist in integrations
			fields[bugIDField] = b.Id().String()
		}

		// create bug
		result, err := client.CreateIssue(
			je.project.ID, createOp.Title, createOp.Message, fields)
		if err != nil {
			err := errors.Wrap(err, "exporting jira issue")
			out <- core.NewExportError(err, b.Id())
			return err
		}

		id := result.ID
		out <- core.NewExportBug(b.Id())
		// mark bug creation operation as exported
		err = markOperationAsExported(
			b, createOp.Id(), id, je.name, time.Time{})
		if err != nil {
			err := errors.Wrap(err, "marking operation as exported")
			out <- core.NewExportError(err, b.Id())
			return err
		}

		// commit operation to avoid creating multiple issues with multiple pushes
		err = b.CommitAsNeeded()
		if err != nil {
			err := errors.Wrap(err, "bug commit")
			out <- core.NewExportError(err, b.Id())
			return err
		}

		// cache bug jira ID
		bugJiraID = id
	}

	// cache operation jira id
	je.cachedOperationIDs[createOp.Id()] = bugJiraID

	for _, op := range snapshot.Operations[1:] {
		// ignore SetMetadata operations
		if _, ok := op.(*bug.SetMetadataOperation); ok {
			continue
		}

		// ignore operations already existing in jira (due to import or export)
		// cache the ID of already exported or imported issues and events from
		// Jira
		if id, ok := op.GetMetadata(metaKeyJiraId); ok {
			je.cachedOperationIDs[op.Id()] = id
			continue
		}

		opAuthor := op.GetAuthor()
		client, err := je.getClientForIdentity(opAuthor.Id())
		if err != nil {
			out <- core.NewExportError(
				fmt.Errorf("missing operation author credentials for user %.8s",
					author.Id().String()), op.Id())
			continue
		}

		var id string
		var exportTime time.Time
		switch opr := op.(type) {
		case *bug.AddCommentOperation:
			comment, err := client.AddComment(bugJiraID, opr.Message)
			if err != nil {
				err := errors.Wrap(err, "adding comment")
				out <- core.NewExportError(err, b.Id())
				return err
			}
			id = comment.ID
			out <- core.NewExportComment(op.Id())

			// cache comment id
			je.cachedOperationIDs[op.Id()] = id

		case *bug.EditCommentOperation:
			if opr.Target == createOp.Id() {
				// An EditCommentOpreation with the Target set to the create operation
				// encodes a modification to the long-description/summary.
				exportTime, err = client.UpdateIssueBody(bugJiraID, opr.Message)
				if err != nil {
					err := errors.Wrap(err, "editing issue")
					out <- core.NewExportError(err, b.Id())
					return err
				}
				out <- core.NewExportCommentEdition(op.Id())
				id = bugJiraID
			} else {
				// Otherwise it's an edit to an actual comment. A comment cannot be
				// edited before it was created, so it must be the case that we have
				// already observed and cached the AddCommentOperation.
				commentID, ok := je.cachedOperationIDs[opr.Target]
				if !ok {
					// Since an edit has to come after the creation, we expect we would
					// have cached the creation id.
					panic("unexpected error: comment id not found")
				}
				comment, err := client.UpdateComment(bugJiraID, commentID, opr.Message)
				if err != nil {
					err := errors.Wrap(err, "editing comment")
					out <- core.NewExportError(err, b.Id())
					return err
				}
				out <- core.NewExportCommentEdition(op.Id())
				// JIRA doesn't track all comment edits, they will only tell us about
				// the most recent one. We must invent a consistent id for the operation
				// so we use the comment ID plus the timestamp of the update, as
				// reported by JIRA. Note that this must be consistent with the importer
				// during ensureComment()
				id = getTimeDerivedID(comment.ID, comment.Updated)
			}

		case *bug.SetStatusOperation:
			jiraStatus, hasStatus := je.statusMap[opr.Status.String()]
			if hasStatus {
				exportTime, err = UpdateIssueStatus(client, bugJiraID, jiraStatus)
				if err != nil {
					err := errors.Wrap(err, "editing status")
					out <- core.NewExportWarning(err, b.Id())
					// Failure to update status isn't necessarily a big error. It's
					// possible that we just don't have enough information to make that
					// update. In this case, just don't export the operation.
					continue
				}
				out <- core.NewExportStatusChange(op.Id())
				id = bugJiraID
			} else {
				out <- core.NewExportError(fmt.Errorf(
					"No jira status mapped for %.8s", opr.Status.String()), b.Id())
			}

		case *bug.SetTitleOperation:
			exportTime, err = client.UpdateIssueTitle(bugJiraID, opr.Title)
			if err != nil {
				err := errors.Wrap(err, "editing title")
				out <- core.NewExportError(err, b.Id())
				return err
			}
			out <- core.NewExportTitleEdition(op.Id())
			id = bugJiraID

		case *bug.LabelChangeOperation:
			exportTime, err = client.UpdateLabels(
				bugJiraID, je.labelsJiraID, opr.Added, opr.Removed)
			if err != nil {
				err := errors.Wrap(err, "updating labels")
				out <- core.NewExportError(err, b.Id())
				return err
			}
			out <- core.NewExportLabelChange(op.Id())
			id = bugJiraID

		default:
			panic("unhandled operation type case")
		}

		// mark operation as exported
		err = markOperationAsExported(
			b, op.Id(), id, je.project.Key, exportTime)
		if err != nil {
			err := errors.Wrap(err, "marking operation as exported")
			out <- core.NewExportError(err, b.Id())
			return err
		}

		// commit at each operation export to avoid exporting same events multiple
		// times
		err = b.CommitAsNeeded()
		if err != nil {
			err := errors.Wrap(err, "bug commit")
			out <- core.NewExportError(err, b.Id())
			return err
		}
	}

	return nil
}

func markOperationAsExported(b *cache.BugCache, target entity.Id, jiraID, name string, exportTime time.Time) error {
	newMetadata := map[string]string{
		metaKeyJiraId:     jiraID,
		metaKeyJiraBridge: name,
	}
	if !exportTime.IsZero() {
		newMetadata[metaKeyJiraExportTime] = exportTime.Format(http.TimeFormat)
	}

	_, err := b.SetMetadata(target, newMetadata)
	return err
}

// UpdateIssueStatus attempts to change the "status" field by finding a
// transition which achieves the desired state and then performing that
// transition
func UpdateIssueStatus(client *Client, issueKeyOrID string, desiredStateNameOrID string) (time.Time, error) {
	var responseTime time.Time

	tlist, err := client.GetTransitions(issueKeyOrID)
	if err != nil {
		return responseTime, err
	}

	transition := getTransitionTo(tlist, desiredStateNameOrID)
	if transition == nil {
		return responseTime, errTransitionNotFound
	}

	responseTime, err = client.DoTransition(issueKeyOrID, transition.ID)
	if err != nil {
		return responseTime, err
	}

	return responseTime, nil
}
