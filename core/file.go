package core

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/dagger/dagger/core/pipeline"
	"github.com/dagger/dagger/core/reffs"
	bkclient "github.com/moby/buildkit/client"
	"github.com/moby/buildkit/client/llb"
	bkgw "github.com/moby/buildkit/frontend/gateway/client"
	"github.com/moby/buildkit/solver/pb"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
	fstypes "github.com/tonistiigi/fsutil/types"
)

// File is a content-addressed file.
type File struct {
	LLB      *pb.Definition `json:"llb"`
	File     string         `json:"file"`
	Pipeline pipeline.Path  `json:"pipeline"`
	Platform specs.Platform `json:"platform"`

	// Services necessary to provision the file.
	Services ServiceBindings `json:"services,omitempty"`
}

func NewFile(ctx context.Context, st llb.State, file string, pipeline pipeline.Path, platform specs.Platform, services ServiceBindings) (*File, error) {
	def, err := st.Marshal(ctx, llb.Platform(platform))
	if err != nil {
		return nil, err
	}

	return &File{
		LLB:      def.ToPB(),
		File:     file,
		Pipeline: pipeline,
		Platform: platform,
		Services: services,
	}, nil
}

// Clone returns a deep copy of the container suitable for modifying in a
// WithXXX method.
func (file *File) Clone() *File {
	cp := *file
	cp.Pipeline = clone(cp.Pipeline)
	cp.Services = cloneMap(cp.Services)
	return &cp
}

// FileID is an opaque value representing a content-addressed file.
type FileID string

// ID marshals the file into a content-addressed ID.
func (file *File) ID() (FileID, error) {
	return encodeID[FileID](file)
}

func (id FileID) ToFile() (*File, error) {
	var file File
	if err := decodeID(&file, id); err != nil {
		return nil, err
	}

	return &file, nil
}

func (file *File) State() (llb.State, error) {
	return defToState(file.LLB)
}

func (file *File) Contents(ctx context.Context, gw bkgw.Client) ([]byte, error) {
	return WithServices(ctx, gw, file.Services, func() ([]byte, error) {
		ref, err := gwRef(ctx, gw, file.LLB)
		if err != nil {
			return nil, err
		}

		return ref.ReadFile(ctx, bkgw.ReadRequest{
			Filename: file.File,
		})
	})
}

func (file *File) Secret(ctx context.Context) (*Secret, error) {
	id, err := file.ID()
	if err != nil {
		return nil, err
	}

	return NewSecretFromFile(id), nil
}

func (file *File) Stat(ctx context.Context, gw bkgw.Client) (*fstypes.Stat, error) {
	return WithServices(ctx, gw, file.Services, func() (*fstypes.Stat, error) {
		ref, err := gwRef(ctx, gw, file.LLB)
		if err != nil {
			return nil, err
		}

		return ref.StatFile(ctx, bkgw.StatRequest{
			Path: file.File,
		})
	})
}

func (file *File) WithTimestamps(ctx context.Context, unix int) (*File, error) {
	file = file.Clone()

	st, err := file.State()
	if err != nil {
		return nil, err
	}

	t := time.Unix(int64(unix), 0)

	stamped := llb.Scratch().File(
		llb.Copy(st, file.File, ".", llb.WithCreatedTime(t)),
		file.Pipeline.LLBOpt(),
	)

	def, err := stamped.Marshal(ctx, llb.Platform(file.Platform))
	if err != nil {
		return nil, err
	}
	file.LLB = def.ToPB()
	file.File = path.Base(file.File)

	return file, nil
}

func (file *File) Open(ctx context.Context, host *Host, gw bkgw.Client) (io.ReadCloser, error) {
	return WithServices(ctx, gw, file.Services, func() (io.ReadCloser, error) {
		fs, err := reffs.OpenDef(ctx, gw, file.LLB)
		if err != nil {
			return nil, err
		}

		return fs.Open(file.File)
	})
}

func (file *File) Export(
	ctx context.Context,
	host *Host,
	dest string,
	bkClient *bkclient.Client,
	solveOpts bkclient.SolveOpt,
	solveCh chan<- *bkclient.SolveStatus,
) error {
	dest, err := host.NormalizeDest(dest)
	if err != nil {
		return err
	}

	if stat, err := os.Stat(dest); err == nil {
		if stat.IsDir() {
			return fmt.Errorf("destination %q is a directory; must be a file path", dest)
		}
	}

	destFilename := filepath.Base(dest)
	destDir := filepath.Dir(dest)

	return host.Export(ctx, bkclient.ExportEntry{
		Type:      bkclient.ExporterLocal,
		OutputDir: destDir,
	}, bkClient, solveOpts, solveCh, func(ctx context.Context, gw bkgw.Client) (*bkgw.Result, error) {
		return WithServices(ctx, gw, file.Services, func() (*bkgw.Result, error) {
			src, err := file.State()
			if err != nil {
				return nil, err
			}

			src = llb.Scratch().File(llb.Copy(src, file.File, destFilename), file.Pipeline.LLBOpt())

			def, err := src.Marshal(ctx, llb.Platform(file.Platform))
			if err != nil {
				return nil, err
			}

			return gw.Solve(ctx, bkgw.SolveRequest{
				Evaluate:   true,
				Definition: def.ToPB(),
			})
		})
	})
}

// gwRef returns the buildkit reference from the solved def.
func gwRef(ctx context.Context, gw bkgw.Client, def *pb.Definition) (bkgw.Reference, error) {
	res, err := gw.Solve(ctx, bkgw.SolveRequest{
		Definition: def,
	})
	if err != nil {
		return nil, err
	}

	ref, err := res.SingleRef()
	if err != nil {
		return nil, err
	}

	if ref == nil {
		// empty file, i.e. llb.Scratch()
		return nil, fmt.Errorf("empty reference")
	}

	return ref, nil
}
