package util

import (
	"bufio"
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pkg/errors"
	"golang.org/x/sys/unix"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	cdiv1 "kubevirt.io/containerized-data-importer-api/pkg/apis/core/v1beta1"
	"kubevirt.io/containerized-data-importer/pkg/common"
)

const (
	blockdevFileName = "/usr/sbin/blockdev"
	// DefaultAlignBlockSize is the alignment size we use to align disk images, its a multiple of all known hardware block sizes 512/4k/8k/32k/64k.
	DefaultAlignBlockSize = 1024 * 1024
)

// CountingReader is a reader that keeps track of how much has been read
type CountingReader struct {
	Reader  io.ReadCloser
	Current uint64
	Done    bool
}

// VddkInfo holds VDDK version and connection information returned by an importer pod
type VddkInfo struct {
	Version string
	Host    string
}

// RandAlphaNum provides an implementation to generate a random alpha numeric string of the specified length
func RandAlphaNum(n int) string {
	rand.Seed(time.Now().UnixNano())
	var letter = []rune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789")
	b := make([]rune, n)
	for i := range b {
		b[i] = letter[rand.Intn(len(letter))]
	}
	return string(b)
}

// GetNamespace returns the namespace the pod is executing in
func GetNamespace() string {
	return getNamespace("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
}

func getNamespace(path string) string {
	if data, err := ioutil.ReadFile(path); err == nil {
		if ns := strings.TrimSpace(string(data)); len(ns) > 0 {
			return ns
		}
	}
	return "cdi"
}

// ParseEnvVar provides a wrapper to attempt to fetch the specified env var
func ParseEnvVar(envVarName string, decode bool) (string, error) {
	value := os.Getenv(envVarName)
	if decode {
		v, err := base64.StdEncoding.DecodeString(value)
		if err != nil {
			return "", errors.Errorf("error decoding environment variable %q", envVarName)
		}
		value = fmt.Sprintf("%s", v)
	}
	return value, nil
}

// Read reads bytes from the stream and updates the prometheus clone_progress metric according to the progress.
func (r *CountingReader) Read(p []byte) (n int, err error) {
	n, err = r.Reader.Read(p)
	r.Current += uint64(n)
	r.Done = err == io.EOF
	return n, err
}

// Close closes the stream
func (r *CountingReader) Close() error {
	return r.Reader.Close()
}

// GetAvailableSpaceByVolumeMode calls another method based on the volumeMode parameter to get the amount of
// available space at the path specified.
func GetAvailableSpaceByVolumeMode(volumeMode v1.PersistentVolumeMode) (int64, error) {
	if volumeMode == v1.PersistentVolumeBlock {
		return GetAvailableSpaceBlock(common.WriteBlockPath)
	}
	return GetAvailableSpace(common.ImporterVolumePath)
}

// GetAvailableSpace gets the amount of available space at the path specified.
func GetAvailableSpace(path string) (int64, error) {
	var stat syscall.Statfs_t
	err := syscall.Statfs(path, &stat)
	if err != nil {
		return int64(-1), err
	}
	return int64(stat.Bavail) * int64(stat.Bsize), nil
}

// GetAvailableSpaceBlock gets the amount of available space at the block device path specified.
func GetAvailableSpaceBlock(deviceName string) (int64, error) {
	// Check if the file exists and is a device file.
	info, err := os.Stat(deviceName)
	if os.IsNotExist(err) {
		return int64(-1), nil
	}
	if !isDevice(info.Mode()) {
		return int64(-1), nil
	}
	// Device exists, attempt to get size.
	cmd := exec.Command(blockdevFileName, "--getsize64", deviceName)
	var out bytes.Buffer
	var errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err = cmd.Run()
	if err != nil {
		return int64(-1), errors.Errorf("%v, %s", err, errBuf.String())
	}
	i, err := strconv.ParseInt(strings.TrimSpace(out.String()), 10, 64)
	if err != nil {
		return int64(-1), err
	}
	return i, nil
}

// isDevice returns true if it's a device file
func isDevice(fileMode os.FileMode) bool {
	if (fileMode & os.ModeDevice) != 0 {
		return true
	}
	return false
}

// MinQuantity calculates the minimum of two quantities.
func MinQuantity(availableSpace, imageSize *resource.Quantity) resource.Quantity {
	if imageSize.Cmp(*availableSpace) == 1 {
		return *availableSpace
	}
	return *imageSize
}

// OpenFileOrBlockDevice opens the destination data file, whether it is a block device or regular file
func OpenFileOrBlockDevice(fileName string) (*os.File, error) {
	var outFile *os.File
	blockSize, err := GetAvailableSpaceBlock(fileName)
	if err != nil {
		return nil, errors.Wrapf(err, "error determining if block device exists")
	}
	if blockSize >= 0 {
		// Block device found and size determined.
		outFile, err = os.OpenFile(fileName, os.O_EXCL|os.O_WRONLY, os.ModePerm)
	} else {
		// Attempt to create the file with name filePath.  If it exists, fail.
		outFile, err = os.OpenFile(fileName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.ModePerm)
	}
	if err != nil {
		return nil, errors.Wrapf(err, "could not open file %q", fileName)
	}
	return outFile, nil
}

// StreamDataToFile provides a function to stream the specified io.Reader to the specified local file
func StreamDataToFile(r io.Reader, fileName string) error {
	outFile, err := OpenFileOrBlockDevice(fileName)
	if err != nil {
		return err
	}
	defer outFile.Close()
	klog.V(1).Infof("Writing data...\n")
	if _, err = io.Copy(outFile, r); err != nil {
		klog.Errorf("Unable to write file from dataReader: %v\n", err)
		os.Remove(outFile.Name())
		return errors.Wrapf(err, "unable to write to file")
	}
	err = outFile.Sync()
	return err
}

// UnArchiveTar unarchives a tar file and streams its files
// using the specified io.Reader to the specified destination.
func UnArchiveTar(reader io.Reader, destDir string, arg ...string) error {
	klog.V(1).Infof("begin untar to %s...\n", destDir)

	var tarOptions string
	var args = arg
	if len(arg) > 0 {
		tarOptions = arg[0]
		args = arg[1:]
	}
	options := fmt.Sprintf("-%s%s", tarOptions, "xvC")
	untar := exec.Command("/usr/bin/tar", options, destDir, strings.Join(args, ""))
	untar.Stdin = reader
	var errBuf bytes.Buffer
	untar.Stderr = &errBuf
	err := untar.Start()
	if err != nil {
		return err
	}
	err = untar.Wait()
	if err != nil {
		klog.V(3).Infof("%s\n", errBuf.String())
		klog.Errorf("%s\n", err.Error())
		return err
	}
	return nil
}

// CopyFile copies a file from one location to another.
func CopyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	if err != nil {
		return err
	}
	return out.Close()
}

// WriteTerminationMessage writes the passed in message to the default termination message file
func WriteTerminationMessage(message string) error {
	return WriteTerminationMessageToFile(common.PodTerminationMessageFile, message)
}

// WriteTerminationMessageToFile writes the passed in message to the passed in message file
func WriteTerminationMessageToFile(file, message string) error {
	message = strings.ReplaceAll(message, "\n", " ")
	// Only write the first line of the message.
	scanner := bufio.NewScanner(strings.NewReader(message))
	if scanner.Scan() {
		err := ioutil.WriteFile(file, []byte(scanner.Text()), os.ModeAppend)
		if err != nil {
			return errors.Wrap(err, "could not create termination message file")
		}
	}
	return nil
}

// CopyDir copies a dir from one location to another.
func CopyDir(source string, dest string) (err error) {
	// get properties of source dir
	sourceinfo, err := os.Stat(source)
	if err != nil {
		return err
	}

	// create dest dir
	err = os.MkdirAll(dest, sourceinfo.Mode())
	if err != nil {
		return err
	}

	directory, _ := os.Open(source)
	objects, err := directory.Readdir(-1)

	for _, obj := range objects {
		src := filepath.Join(source, obj.Name())
		dst := filepath.Join(dest, obj.Name())

		if obj.IsDir() {
			// create sub-directories - recursively
			err = CopyDir(src, dst)
			if err != nil {
				fmt.Println(err)
			}
		} else {
			// perform copy
			err = CopyFile(src, dst)
			if err != nil {
				fmt.Println(err)
			}
		}
	}
	return
}

// LinkFile symlinks the source to the target
func LinkFile(source, target string) error {
	out, err := exec.Command("/usr/bin/ln", "-s", source, target).CombinedOutput()
	if err != nil {
		fmt.Printf("out [%s]\n", string(out))
		return err
	}
	return nil
}

// RoundDown returns the number rounded down to the nearest multiple.
func RoundDown(number, multiple int64) int64 {
	return number / multiple * multiple
}

// RoundUp returns the number rounded up to the nearest multiple.
func RoundUp(number, multiple int64) int64 {
	partitions := math.Ceil(float64(number) / float64(multiple))
	return int64(partitions) * multiple
}

// MergeLabels adds source labels to destination (does not change existing ones)
func MergeLabels(src, dest map[string]string) map[string]string {
	if dest == nil {
		dest = map[string]string{}
	}

	for k, v := range src {
		dest[k] = v
	}

	return dest
}

// GetRecommendedInstallerLabelsFromCr returns the recommended labels to set on CDI resources
func GetRecommendedInstallerLabelsFromCr(cr *cdiv1.CDI) map[string]string {
	labels := map[string]string{}

	// In non-standalone installs, we fetch labels that were set on the CDI CR by the installer
	for k, v := range cr.GetLabels() {
		if k == common.AppKubernetesPartOfLabel || k == common.AppKubernetesVersionLabel {
			labels[k] = v
		}
	}

	return labels
}

// SetRecommendedLabels sets the recommended labels on CDI resources (does not get rid of existing ones)
func SetRecommendedLabels(obj metav1.Object, installerLabels map[string]string, controllerName string) {
	staticLabels := map[string]string{
		common.AppKubernetesManagedByLabel: controllerName,
		common.AppKubernetesComponentLabel: "storage",
	}

	// Merge static & existing labels
	mergedLabels := MergeLabels(staticLabels, obj.GetLabels())
	// Add installer dynamic labels as well (/version, /part-of)
	mergedLabels = MergeLabels(installerLabels, mergedLabels)

	obj.SetLabels(mergedLabels)
}

// Md5sum calculates the md5sum of a given file
func Md5sum(filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer file.Close()

	hash := md5.New()

	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}

	hashInBytes := hash.Sum(nil)[:16]
	return hex.EncodeToString(hashInBytes), nil
}

// Three functions for zeroing a range in the destination file:

// PunchHole attempts to zero a range in a file with fallocate, for block devices and pre-allocated files.
func PunchHole(outFile *os.File, start, length int64) error {
	klog.Infof("Punching %d-byte hole at offset %d", length, start)
	flags := uint32(unix.FALLOC_FL_PUNCH_HOLE | unix.FALLOC_FL_KEEP_SIZE)
	err := syscall.Fallocate(int(outFile.Fd()), flags, start, length)
	if err == nil {
		_, err = outFile.Seek(length, io.SeekCurrent) // Just to move current file position
	}
	return err
}

// AppendZeroWithTruncate resizes the file to append zeroes, meant only for newly-created (empty and zero-length) regular files.
func AppendZeroWithTruncate(outFile *os.File, start, length int64) error {
	klog.Infof("Truncating %d-bytes from offset %d", length, start)
	end, err := outFile.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	if start != end {
		return errors.Errorf("starting offset %d does not match previous ending offset %d, cannot safely append zeroes to this file using truncate", start, end)
	}
	err = outFile.Truncate(start + length)
	if err != nil {
		return err
	}
	_, err = outFile.Seek(0, io.SeekEnd)
	return err
}

var zeroBuffer []byte

// AppendZeroWithWrite just does normal file writes to the destination, a slow but reliable fallback option.
func AppendZeroWithWrite(outFile *os.File, start, length int64) error {
	klog.Infof("Writing %d zero bytes at offset %d", length, start)
	offset, err := outFile.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	if start != offset {
		return errors.Errorf("starting offset %d does not match previous ending offset %d, cannot safely append zeroes to this file using write", start, offset)
	}
	if zeroBuffer == nil { // No need to re-allocate this on every write
		zeroBuffer = bytes.Repeat([]byte{0}, 32<<20)
	}
	count := int64(0)
	for count < length {
		blockSize := int64(len(zeroBuffer))
		remaining := length - count
		if remaining < blockSize {
			blockSize = remaining
		}
		written, err := outFile.Write(zeroBuffer[:blockSize])
		if err != nil {
			return errors.Wrapf(err, "unable to write %d zeroes at offset %d: %v", length, start+count, err)
		}
		count += int64(written)
	}
	return nil
}

// GetUsableSpace calculates space to use taking file system overhead into account
func GetUsableSpace(filesystemOverhead float64, availableSpace int64) int64 {
	// +1 always rounds up.
	spaceWithOverhead := int64(math.Ceil((1 - filesystemOverhead) * float64(availableSpace)))
	// qemu-img will round up, making us use more than the usable space.
	// This later conflicts with image size validation.
	return RoundDown(spaceWithOverhead, DefaultAlignBlockSize)
}

// ResolveVolumeMode returns the volume mode if set, otherwise defaults to file system mode
func ResolveVolumeMode(volumeMode *v1.PersistentVolumeMode) v1.PersistentVolumeMode {
	retVolumeMode := v1.PersistentVolumeFilesystem
	if volumeMode != nil && *volumeMode == v1.PersistentVolumeBlock {
		retVolumeMode = v1.PersistentVolumeBlock
	}
	return retVolumeMode
}
