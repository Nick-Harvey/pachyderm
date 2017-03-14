package server

import (
	"fmt"
	"math/rand"
	"path"
	"sync/atomic"
	"testing"

	"github.com/pachyderm/pachyderm/src/client"
	pfsclient "github.com/pachyderm/pachyderm/src/client/pfs"
	"github.com/pachyderm/pachyderm/src/client/pkg/require"
	"github.com/pachyderm/pachyderm/src/client/pkg/uuid"
	ppsclient "github.com/pachyderm/pachyderm/src/client/pps"
	"github.com/pachyderm/pachyderm/src/server/pkg/workload"
	"golang.org/x/sync/errgroup"
)

const (
	KB               = 1024
	MB               = 1024 * KB
	GB               = 1024 * MB
	lettersAndSpaces = "abcdefghijklmnopqrstuvwxyz      "
)

// countWriter increments a counter by the number of bytes given
type countWriter struct {
	count int64
}

func (w *countWriter) Write(p []byte) (int, error) {
	atomic.AddInt64(&w.count, int64(len(p)))
	return len(p), nil
}

func getRand() *rand.Rand {
	source := rand.Int63()
	return rand.New(rand.NewSource(source))
}

func BenchmarkManySmallFiles(b *testing.B) {
	benchmarkFiles(b, 1000000, 500, 1*KB)
}

func BenchmarkSomeLargeFiles(b *testing.B) {
	benchmarkFiles(b, 1000, 500*MB, 1*GB)
}

// benchmarkFiles runs a benchmarks that uploads, downloads, and processes
// fileNum files, whose sizes (in bytes) are produced by a normal
// distribution with the given standard deviation and mean.
func benchmarkFiles(b *testing.B, fileNum int, stdDev int64, mean int64) {
	repo := "BenchmarkPachyderm" + uuid.NewWithoutDashes()[0:12]
	c, err := client.NewFromAddress("127.0.0.1:30650")
	require.NoError(b, err)
	require.NoError(b, c.CreateRepo(repo))

	commit, err := c.StartCommit(repo, "")
	require.NoError(b, err)
	if !b.Run(fmt.Sprintf("Put%dFiles", fileNum), func(b *testing.B) {
		var totalBytes int64
		var eg errgroup.Group
		for k := 0; k < fileNum; k++ {
			k := k
			eg.Go(func() error {
				r := getRand()
				fileSize := int64(r.NormFloat64()*float64(stdDev) + float64(mean))
				atomic.AddInt64(&totalBytes, fileSize)
				_, err := c.PutFile(repo, commit.ID, fmt.Sprintf("file%d", k), workload.NewReader(r, fileSize))
				return err
			})
		}
		require.NoError(b, eg.Wait())
		b.SetBytes(totalBytes)
	}) {
		return
	}
	require.NoError(b, c.FinishCommit(repo, commit.ID))
	require.NoError(b, c.SetBranch(repo, commit.ID, "master"))

	if !b.Run(fmt.Sprintf("Get%dFiles", fileNum), func(b *testing.B) {
		w := &countWriter{}
		var eg errgroup.Group
		for k := 0; k < fileNum; k++ {
			k := k
			eg.Go(func() error {
				return c.GetFile(repo, commit.ID, fmt.Sprintf("file%d", k), 0, 0, w)
			})
		}
		require.NoError(b, eg.Wait())
		defer func() { b.SetBytes(w.count) }()
	}) {
		return
	}

	if !b.Run(fmt.Sprintf("PipelineCopy%dFiles", fileNum), func(b *testing.B) {
		pipeline := "BenchmarkPachydermPipeline" + uuid.NewWithoutDashes()[0:12]
		require.NoError(b, c.CreatePipeline(
			pipeline,
			"",
			[]string{"bash"},
			[]string{fmt.Sprintf("cp -R %s /pfs/out/", path.Join("/pfs", repo, "/*"))},
			&ppsclient.ParallelismSpec{
				Strategy: ppsclient.ParallelismSpec_CONSTANT,
				Constant: 4,
			},
			[]*ppsclient.PipelineInput{{
				Repo: client.NewRepo(repo),
				Glob: "/*",
			}},
			"",
			false,
		))
		_, err := c.FlushCommit([]*pfsclient.Commit{client.NewCommit(repo, commit.ID)}, nil)
		require.NoError(b, err)
		b.StopTimer()
		repoInfo, err := c.InspectRepo(repo)
		require.NoError(b, err)
		b.SetBytes(int64(repoInfo.SizeBytes))
		repoInfo, err = c.InspectRepo(pipeline)
		require.NoError(b, err)
		b.SetBytes(int64(repoInfo.SizeBytes))
	}) {
		return
	}
}
