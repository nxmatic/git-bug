package repository

import (
	"log"
	"math/rand"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/MichaelMure/git-bug/util/lamport"
)

func CleanupTestRepos(repos ...Repo) {
	var firstErr error
	for _, repo := range repos {
		if repo, ok := repo.(TestedRepo); ok {
			err := repo.EraseFromDisk()
			if err != nil {
				log.Println(err)
				if firstErr == nil {
					firstErr = err
				}
			}
		}
	}

	if firstErr != nil {
		log.Fatal(firstErr)
	}
}

type RepoCreator func(bare bool) TestedRepo
type RepoCleaner func(repos ...Repo)

// Test suite for a Repo implementation
func RepoTest(t *testing.T, creator RepoCreator, cleaner RepoCleaner) {
	for bare, name := range map[bool]string{
		false: "Plain",
		true:  "Bare",
	} {
		t.Run(name, func(t *testing.T) {
			repo := creator(bare)
			defer cleaner(repo)

			t.Run("Data", func(t *testing.T) {
				RepoDataTest(t, repo)
			})

			t.Run("Config", func(t *testing.T) {
				RepoConfigTest(t, repo)
			})

			t.Run("Clocks", func(t *testing.T) {
				RepoClockTest(t, repo)
			})
		})
	}
}

// helper to test a RepoConfig
func RepoConfigTest(t *testing.T, repo RepoConfig) {
	testConfig(t, repo.LocalConfig())
}

// helper to test a RepoData
func RepoDataTest(t *testing.T, repo RepoData) {
	// Blob

	data := randomData()

	blobHash1, err := repo.StoreData(data)
	require.NoError(t, err)
	require.True(t, blobHash1.IsValid())

	blob1Read, err := repo.ReadData(blobHash1)
	require.NoError(t, err)
	require.Equal(t, data, blob1Read)

	// Tree

	blobHash2, err := repo.StoreData(randomData())
	require.NoError(t, err)
	blobHash3, err := repo.StoreData(randomData())
	require.NoError(t, err)

	tree1 := []TreeEntry{
		{
			ObjectType: Blob,
			Hash:       blobHash1,
			Name:       "blob1",
		},
		{
			ObjectType: Blob,
			Hash:       blobHash2,
			Name:       "blob2",
		},
	}

	treeHash1, err := repo.StoreTree(tree1)
	require.NoError(t, err)
	require.True(t, treeHash1.IsValid())

	tree1Read, err := repo.ReadTree(treeHash1)
	require.NoError(t, err)
	require.ElementsMatch(t, tree1, tree1Read)

	tree2 := []TreeEntry{
		{
			ObjectType: Tree,
			Hash:       treeHash1,
			Name:       "tree1",
		},
		{
			ObjectType: Blob,
			Hash:       blobHash3,
			Name:       "blob3",
		},
	}

	treeHash2, err := repo.StoreTree(tree2)
	require.NoError(t, err)
	require.True(t, treeHash2.IsValid())

	tree2Read, err := repo.ReadTree(treeHash2)
	require.NoError(t, err)
	require.ElementsMatch(t, tree2, tree2Read)

	// Commit

	commit1, err := repo.StoreCommit(treeHash1)
	require.NoError(t, err)
	require.True(t, commit1.IsValid())

	treeHash1Read, err := repo.GetTreeHash(commit1)
	require.NoError(t, err)
	require.Equal(t, treeHash1, treeHash1Read)

	commit2, err := repo.StoreCommitWithParent(treeHash2, commit1)
	require.NoError(t, err)
	require.True(t, commit2.IsValid())

	treeHash2Read, err := repo.GetTreeHash(commit2)
	require.NoError(t, err)
	require.Equal(t, treeHash2, treeHash2Read)

	// ReadTree should accept tree and commit hashes
	tree1read, err := repo.ReadTree(commit1)
	require.NoError(t, err)
	require.Equal(t, tree1read, tree1)

	// Ref

	exist1, err := repo.RefExist("refs/bugs/ref1")
	require.NoError(t, err)
	require.False(t, exist1)

	err = repo.UpdateRef("refs/bugs/ref1", commit2)
	require.NoError(t, err)

	exist1, err = repo.RefExist("refs/bugs/ref1")
	require.NoError(t, err)
	require.True(t, exist1)

	ls, err := repo.ListRefs("refs/bugs")
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"refs/bugs/ref1"}, ls)

	err = repo.CopyRef("refs/bugs/ref1", "refs/bugs/ref2")
	require.NoError(t, err)

	ls, err = repo.ListRefs("refs/bugs")
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"refs/bugs/ref1", "refs/bugs/ref2"}, ls)

	commits, err := repo.ListCommits("refs/bugs/ref2")
	require.NoError(t, err)
	require.Equal(t, []Hash{commit1, commit2}, commits)

	// Graph

	commit3, err := repo.StoreCommitWithParent(treeHash1, commit1)
	require.NoError(t, err)

	ancestorHash, err := repo.FindCommonAncestor(commit2, commit3)
	require.NoError(t, err)
	require.Equal(t, commit1, ancestorHash)

	err = repo.RemoveRef("refs/bugs/ref1")
	require.NoError(t, err)
}

// helper to test a RepoClock
func RepoClockTest(t *testing.T, repo RepoClock) {
	clock, err := repo.GetOrCreateClock("foo")
	require.NoError(t, err)
	require.Equal(t, lamport.Time(1), clock.Time())

	time, err := clock.Increment()
	require.NoError(t, err)
	require.Equal(t, lamport.Time(1), time)
	require.Equal(t, lamport.Time(2), clock.Time())

	clock2, err := repo.GetOrCreateClock("foo")
	require.NoError(t, err)
	require.Equal(t, lamport.Time(2), clock2.Time())

	clock3, err := repo.GetOrCreateClock("bar")
	require.NoError(t, err)
	require.Equal(t, lamport.Time(1), clock3.Time())
}

func randomData() []byte {
	var letterRunes = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	b := make([]byte, 32)
	for i := range b {
		b[i] = letterRunes[rand.Intn(len(letterRunes))]
	}
	return b
}
