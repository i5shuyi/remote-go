//go:build !darwin

package liveupdate

func stageHelper(src, dir string) (string, error) {
	return stageExecutable(src, dir)
}
