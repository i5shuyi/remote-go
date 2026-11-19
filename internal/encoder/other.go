//go:build !(darwin && cgo) && !(windows && (amd64 || arm64))

package encoder

func startH264(Options) (Video, error) { return nil, ErrUnsupported }
