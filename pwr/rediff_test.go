package pwr

import (
	"bytes"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/Datadog/zstd"
	"github.com/alecthomas/assert"
	humanize "github.com/dustin/go-humanize"
	"github.com/itchio/wharf/pools/fspool"
	"github.com/itchio/wharf/state"
	"github.com/itchio/wharf/tlc"
)

type zstdCompressor struct{}

func (zc *zstdCompressor) Apply(writer io.Writer, quality int32) (io.Writer, error) {
	return zstd.NewWriterLevel(writer, int(quality)), nil
}

type zstdDecompressor struct{}

func (zd *zstdDecompressor) Apply(reader io.Reader) (io.Reader, error) {
	return zstd.NewReader(reader), nil
}

func init() {
	RegisterCompressor(CompressionAlgorithm_ZSTD, &zstdCompressor{})
	RegisterDecompressor(CompressionAlgorithm_ZSTD, &zstdDecompressor{})
}

func Test_RediffWorse(t *testing.T) {
	runRediffScenario(t, patchScenario{
		name:         "rediff gets slightly worse",
		touchedFiles: 1,
		deletedFiles: 0,
		v1: testDirSettings{
			entries: []testDirEntry{
				{path: "subdir/file-1", seed: 0x1, size: BlockSize*21 + 14},
				{path: "file-1", seed: 0x2},
				{path: "dir2/file-2", seed: 0x3},
			},
		},
		v2: testDirSettings{
			entries: []testDirEntry{
				{path: "subdir/file-1", seed: 0x1, size: BlockSize*27 + 14},
				{path: "file-1", seed: 0x2},
				{path: "dir2/file-2", seed: 0x33},
			},
		},
	})
}

func Test_RediffBetter(t *testing.T) {
	runRediffScenario(t, patchScenario{
		name:         "rediff gets better!",
		touchedFiles: 1,
		deletedFiles: 0,
		v1: testDirSettings{
			entries: []testDirEntry{
				{path: "subdir/file-1", seed: 0x1, size: BlockSize*21 + 14},
				{path: "file-1", seed: 0x2},
				{path: "dir2/file-2", seed: 0x3},
			},
		},
		v2: testDirSettings{
			entries: []testDirEntry{
				{path: "subdir/file-1", seed: 0x1, size: BlockSize*21 + 14, bsmods: []bsmod{
					bsmod{interval: BlockSize/2 + 3, delta: 0x4},
					bsmod{interval: BlockSize/3 + 7, delta: 0x18},
				}},
				{path: "file-1", seed: 0x2},
				{path: "dir2/file-2", seed: 0x33},
			},
		},
	})
}

func Test_RediffStillBetter(t *testing.T) {
	runRediffScenario(t, patchScenario{
		name:         "rediff gets better even though rsync wasn't that bad",
		touchedFiles: 1,
		deletedFiles: 0,
		v1: testDirSettings{
			entries: []testDirEntry{
				{path: "subdir/file-1", seed: 0x1, size: BlockSize*58 + 14},
				{path: "file-1", seed: 0x2, size: BlockSize * 16},
				{path: "dir2/file-2", seed: 0x3},
			},
		},
		v2: testDirSettings{
			entries: []testDirEntry{
				{path: "subdir/file-1", seed: 0x1, size: BlockSize * 61, bsmods: []bsmod{
					bsmod{interval: BlockSize/2 + 3, delta: 0x4, max: 4, skip: 20},
					bsmod{interval: BlockSize/3 + 7, delta: 0x18, max: 6, skip: 20},
				}},
				{path: "file-1", chunks: []testDirChunk{
					testDirChunk{size: BlockSize*8 + 3, seed: 0x99},
					testDirChunk{size: BlockSize*7 + 12, seed: 0x2},
				}},
				{path: "dir2/file-2", seed: 0x33},
			},
		},
	})
}

func runRediffScenario(t *testing.T, scenario patchScenario) {
	log := t.Logf

	mainDir, err := ioutil.TempDir("", "rediff")
	assert.NoError(t, err)
	assert.NoError(t, os.MkdirAll(mainDir, 0755))
	defer os.RemoveAll(mainDir)

	v1 := filepath.Join(mainDir, "v1")
	makeTestDir(t, v1, scenario.v1)

	v2 := filepath.Join(mainDir, "v2")
	makeTestDir(t, v2, scenario.v2)

	compression := &CompressionSettings{}
	compression.Algorithm = CompressionAlgorithm_ZSTD
	compression.Quality = 1

	sourceContainer, err := tlc.WalkAny(v2, nil)
	assert.NoError(t, err)

	consumer := &state.Consumer{
		OnMessage: func(level string, message string) {
			// log("[%s] %s", level, message)
		},
	}
	patchBuffer := new(bytes.Buffer)
	signatureBuffer := new(bytes.Buffer)

	func() {
		targetContainer, dErr := tlc.WalkAny(v1, nil)
		assert.NoError(t, dErr)

		targetPool := fspool.New(targetContainer, v1)
		targetSignature, dErr := ComputeSignature(targetContainer, targetPool, consumer)
		assert.NoError(t, dErr)

		pool := fspool.New(sourceContainer, v2)

		dctx := &DiffContext{
			Compression: compression,
			Consumer:    consumer,

			SourceContainer: sourceContainer,
			Pool:            pool,

			TargetContainer: targetContainer,
			TargetSignature: targetSignature,
		}

		assert.NoError(t, dctx.WritePatch(patchBuffer, signatureBuffer))
	}()

	signature, sErr := ReadSignature(bytes.NewReader(signatureBuffer.Bytes()))
	assert.NoError(t, sErr)

	v1Before := filepath.Join(mainDir, "v1Before")
	cpDir(t, v1, v1Before)

	v1After := filepath.Join(mainDir, "v1After")

	assert.NoError(t, os.RemoveAll(v1Before))
	cpDir(t, v1, v1Before)

	func() {
		actx := &ApplyContext{
			TargetPath: v1Before,
			OutputPath: v1After,

			Consumer: consumer,
		}

		patchReader := bytes.NewReader(patchBuffer.Bytes())

		aErr := actx.ApplyPatch(patchReader)
		assert.NoError(t, aErr)

		signature, sErr := ReadSignature(bytes.NewReader(signatureBuffer.Bytes()))
		assert.NoError(t, sErr)
		assert.NoError(t, AssertValid(v1After, signature))
		log("Original patch applies cleanly.")
	}()

	func() {
		targetContainer, dErr := tlc.WalkAny(v1, nil)
		assert.NoError(t, dErr)

		rc := &RediffContext{
			TargetPool: fspool.New(targetContainer, v1),
			SourcePool: fspool.New(sourceContainer, v2),

			Consumer:              consumer,
			Compression:           compression,
			SuffixSortConcurrency: max(2, runtime.NumCPU()-1),
		}

		patchReader := bytes.NewReader(patchBuffer.Bytes())

		log("Optimizing in parallel (concurrency=%d)...", rc.SuffixSortConcurrency)
		aErr := rc.AnalyzePatch(patchReader)
		assert.NoError(t, aErr)

		patchReader.Seek(0, os.SEEK_SET)
		optimizedPatchBuffer := new(bytes.Buffer)

		beforeParallel := time.Now()
		oErr := rc.OptimizePatch(patchReader, optimizedPatchBuffer)
		assert.NoError(t, oErr)
		parallelTime := time.Since(beforeParallel)

		before := patchBuffer.Len()
		after := optimizedPatchBuffer.Len()

		diff := (float64(after) - float64(before)) / float64(before) * 100
		if diff < 0 {
			log("Patch is %.2f%% smaller (%s < %s)", -diff, humanize.IBytes(uint64(after)), humanize.IBytes(uint64(before)))
		} else {
			log("Patch is %.2f%% larger (%s > %s)", diff, humanize.IBytes(uint64(after)), humanize.IBytes(uint64(before)))
		}

		patchReader.Seek(0, os.SEEK_SET)
		sequentialOptimizedPatchBuffer := new(bytes.Buffer)

		log("Optimizing sequentially...")
		beforeSequential := time.Now()
		rc.SuffixSortConcurrency = 0
		oErr = rc.OptimizePatch(patchReader, sequentialOptimizedPatchBuffer)
		assert.NoError(t, oErr)
		sequentialTime := time.Since(beforeSequential)

		assert.EqualValues(t, optimizedPatchBuffer.Bytes(), sequentialOptimizedPatchBuffer.Bytes())

		diff = (parallelTime.Seconds() - sequentialTime.Seconds()) / sequentialTime.Seconds() * 100
		if diff < 0 {
			log("Parallel was %.2f%% faster (%s < %s)", -diff, parallelTime, sequentialTime)
		} else {
			log("Parallel was %.2f%% slower (%s > %s)", diff, parallelTime, sequentialTime)
		}

		log("Parallel and sequential gave byte-equivalent results!")

		assert.NoError(t, os.RemoveAll(v1Before))
		cpDir(t, v1, v1Before)

		func() {
			actx := &ApplyContext{
				TargetPath: v1Before,
				OutputPath: v1After,

				Consumer: consumer,
			}

			patchReader := bytes.NewReader(optimizedPatchBuffer.Bytes())

			aErr := actx.ApplyPatch(patchReader)
			assert.NoError(t, aErr)

			assert.NoError(t, AssertValid(v1After, signature))
			log("Optimized patch applies cleanly.")
		}()
	}()
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
