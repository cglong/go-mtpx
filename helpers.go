package mtpx

import (
	"errors"
	"fmt"
	"github.com/ganeshrvel/go-mtpfs/mtp"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// fetch the file size of the object
func GetFileSize(dev *mtp.Device, obj *mtp.ObjectInfo, objectId uint32) (int64, error) {
	var size int64
	if obj.CompressedSize == 0xffffffff {
		var val mtp.Uint64Value
		if err := dev.GetObjectPropValue(objectId, mtp.OPC_ObjectSize, &val); err != nil {
			return 0, FileObjectError{
				fmt.Errorf("GetObjectPropValue handle %d failed: %v", objectId, err.Error()),
			}
		}

		size = int64(val.Value)
	} else {
		size = int64(obj.CompressedSize)
	}

	return size, nil
}

// fetch an object using [objectId]
// [parentPath] is required to keep track of the [fullPath] of the object
func GetObjectFromObjectId(dev *mtp.Device, objectId uint32, parentPath string) (*FileInfo, error) {
	obj := mtp.ObjectInfo{}

	// if the [objectId] is root then return the basic root directory information
	if objectId == ParentObjectId {
		return &FileInfo{
			Size:     0xFFFFFFFF,
			IsDir:    true,
			FullPath: "/",
			ObjectId: ParentObjectId,
			Info:     &mtp.ObjectInfo{},
		}, nil
	}

	if err := dev.GetObjectInfo(objectId, &obj); err != nil {
		return nil, FileObjectError{error: err}
	}

	size, _ := GetFileSize(dev, &obj, objectId)
	isDir := isObjectADir(&obj)
	filename := obj.Filename
	_parentPath := fixSlash(parentPath)
	fullPath := getFullPath(_parentPath, filename)

	return &FileInfo{
		Info:       &obj,
		Size:       size,
		IsDir:      isDir,
		ModTime:    obj.ModificationDate,
		Name:       obj.Filename,
		FullPath:   fullPath,
		ParentPath: _parentPath,
		Extension:  extension(obj.Filename, isDir),
		ParentId:   obj.ParentObject,
		ObjectId:   objectId,
	}, nil
}

// fetch the object using [parentId] and [filename]
// it matches the [filename] to the list of files in the directory
// Since the [parentPath] is unavailable here the [fullPath] property of the resulting object [FileInfo] may not be valid.
func GetObjectFromParentIdAndFilename(dev *mtp.Device, storageId uint32, parentId uint32, filename string) (*FileInfo, error) {
	handles := mtp.Uint32Array{}
	if err := dev.GetObjectHandles(storageId, mtp.GOH_ALL_ASSOCS, parentId, &handles); err != nil {
		return nil, FileObjectError{error: err}
	}

	for _, objectId := range handles.Values {
		// fetch the ObjectFileName
		var val mtp.StringValue
		if err := dev.GetObjectPropValue(objectId, mtp.OPC_ObjectFileName, &val); err != nil {
			return nil, FileObjectError{error: err}
		}

		// if the ObjectFileName doesn't match the [filename] then skip the current iteration
		// this will avoid fetching the whole object properties and improve the performance a bit.
		if val.Value != filename {
			continue
		}

		fi, err := GetObjectFromObjectId(dev, objectId, "")

		if err != nil {
			return nil, FileObjectError{error: err}
		}

		// return the current objectId if the filename == fi.Name
		if fi.Name == filename {
			return fi, nil
		}
	}

	return nil, FileNotFoundError{error: fmt.Errorf("file not found: %s", filename)}
}

// fetch the object information using [fullPath]
// Since the [parentPath] is unavailable here the [fullPath] property of the resulting object [FileInfo] may not be valid.
func GetObjectFromPath(dev *mtp.Device, storageId uint32, fullPath string) (*FileInfo, error) {
	if fullPath == "" {
		return nil, InvalidPathError{error: fmt.Errorf("path does not exists. path: %s", fullPath)}
	}

	_filePath := fixSlash(fullPath)

	if _filePath == PathSep {
		return GetObjectFromObjectId(dev, ParentObjectId, "")
	}

	splittedFilePath := strings.Split(_filePath, PathSep)

	var objectId = uint32(ParentObjectId)
	var resultCount = 0
	var fi *FileInfo
	const skipIndex = 1

	for i, fName := range splittedFilePath[skipIndex:] {
		_fi, err := GetObjectFromParentIdAndFilename(dev, storageId, objectId, fName)

		if err != nil {
			switch err.(type) {
			case FileNotFoundError:
				return nil, InvalidPathError{
					error: fmt.Errorf("path not found: %s\nreason: %v", fullPath, err.Error()),
				}

			default:
				return nil, err
			}
		}

		if !_fi.IsDir && indexExists(splittedFilePath, i+1+skipIndex) {
			return nil, InvalidPathError{error: fmt.Errorf("path not found: %s", fullPath)}
		}

		// updating [fi] to current [_fi]
		fi = _fi

		// updating the [objectId] to follow the nested directory
		objectId = _fi.ObjectId

		// keeping a tab on total results
		resultCount += 1
	}

	if resultCount < 1 || fi == nil {
		return nil, InvalidPathError{error: fmt.Errorf("file not found: %s", fullPath)}
	}

	fi.FullPath = _filePath

	return fi, nil
}

// fetch an object using [objectId] and/or [fullPath]
// Since the [parentPath] is unavailable here the [fullPath] property of the resulting object [FileInfo] may not be valid.
func GetObjectFromObjectIdOrPath(dev *mtp.Device, storageId, objectId uint32, fullPath string) (*FileInfo, error) {
	_objectId := objectId

	if _objectId == 0 && fullPath == "" {
		return nil, InvalidPathError{error: fmt.Errorf("invalid path: %s. both objectId and fullPath cannot be empty", fullPath)}
	}

	// if objectId is not available then fetch the objectId from fullPath
	if _objectId == 0 {
		fp, err := GetObjectFromPath(dev, storageId, fullPath)

		if err != nil {
			return nil, err
		}

		return fp, nil
	}

	fo, err := GetObjectFromObjectId(dev, _objectId, fullPath)
	if err != nil {
		return nil, err
	}

	return fo, nil
}

// check if the object is a directory
func isObjectADir(obj *mtp.ObjectInfo) bool {
	return obj.ObjectFormat == mtp.OFC_Association
}

// helper function to create a directory
func handleMakeDirectory(dev *mtp.Device, storageId, parentId uint32, filename string) (rObjectId uint32, rError error) {
	send := mtp.ObjectInfo{
		StorageID:        storageId,
		ObjectFormat:     mtp.OFC_Association,
		ParentObject:     parentId,
		Filename:         filename,
		CompressedSize:   0,
		ModificationDate: time.Now(),
	}

	// create a new object handle
	_, _, objId, err := dev.SendObjectInfo(storageId, parentId, &send)
	if err != nil {
		return 0, SendObjectError{error: err}
	}

	return objId, nil
}

// helper function to create a device file
func handleMakeFile(dev *mtp.Device, storageId uint32, obj *mtp.ObjectInfo, fInfo *os.FileInfo, fileBuf *os.File, overwriteExisting bool, progressCb SizeProgressCb) (rObjectId uint32, rError error) {
	fi, err := GetObjectFromParentIdAndFilename(dev, storageId, obj.ParentObject, obj.Filename)

	// file exists
	if err == nil {
		// if [overwriteExisting] is false then just return existing [objectId] of the exisiting file
		if !overwriteExisting {
			return fi.ObjectId, nil
		}

		// if [overwriteExisting] is true then delete the existing file
		if err := DeleteFile(dev, storageId, fi.ObjectId, ""); err != nil {
			return 0, err
		}
	} else {
		switch err.(type) {
		// if the file does not exists then do nothing
		case FileNotFoundError:

		default:
			return 0, err
		}
	}

	// create a new object handle
	_, _, objId, err := dev.SendObjectInfo(storageId, obj.ParentObject, obj)
	if err != nil {
		return objId, SendObjectError{error: err}
	}

	size := (*fInfo).Size()
	// send the bytes data to the newly create object handle
	err = dev.SendObject(fileBuf, size, func(sent int64) error {
		if err := progressCb(size, sent, objId, nil); err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return objId, SendObjectError{error: err}
	}

	return objId, nil
}

// helper function to create a local file
func handleMakeLocalFile(dev *mtp.Device, fi *FileInfo, destination string) error {
	f, err := os.Create(destination)
	if err != nil {
		return err
	}
	defer f.Close()

	err = dev.GetObject(fi.ObjectId, f)
	if err != nil {
		return err
	}

	return err
}

// helper function to fetch the contents inside a directory
// use [recursive] to fetch the whole nested tree
// [objectId] and [fullPath] are optional parameters
// if [objectId] is not available then [fullPath] will be used to fetch the [objectId]
// dont leave both [objectId] and [fullPath] empty
// Tips: use [objectId] whenever possible to avoid traversing down the whole file tree to process and find the [objectId]
// if [skipDisallowedFiles] is true then files matching the [disallowedFiles] list will be ignored
// returns total number of objects
func proccessWalk(dev *mtp.Device, storageId, objectId uint32, fullPath string, recursive, skipDisallowedFiles bool, cb WalkCb) (rTotalFiles int, rError error) {
	fi, err := GetObjectFromObjectIdOrPath(dev, storageId, objectId, fullPath)

	if err != nil {
		return 0, err
	}

	handles := mtp.Uint32Array{}
	if err := dev.GetObjectHandles(storageId, mtp.GOH_ALL_ASSOCS, fi.ObjectId, &handles); err != nil {
		return 0, ListDirectoryError{error: err}
	}

	totalFiles := 0

	for _, objId := range handles.Values {
		fi, err := GetObjectFromObjectId(dev, objId, fullPath)
		if err != nil {
			continue
		}

		// if the object file name matches [disallowedFiles] list then ignore it
		if skipDisallowedFiles {
			fName := (*fi).Name

			if ok := isDisallowedFiles(fName); ok {
				continue
			}
		}

		totalFiles += 1

		err = cb(objId, fi, nil)
		if err != nil {
			return totalFiles, err
		}

		// don't traverse down the tree if [recursive] is false
		if !recursive {
			continue
		}

		// don't traverse down the tree if the object is not a directory
		if !fi.IsDir {
			continue
		}

		_totalFiles, err := proccessWalk(
			dev, storageId, objId, fi.FullPath, recursive, skipDisallowedFiles, cb,
		)
		if err != nil {
			return totalFiles, err
		}

		totalFiles += _totalFiles
	}

	return totalFiles, nil
}

// create a local directory
func makeLocalDirectory(filename string) error {
	err := os.MkdirAll(filename, os.FileMode(newLocalDirectoryMode))
	if err != nil {
		switch err.(type) {
		case *os.PathError:
			if errors.Is(err, os.ErrPermission) {
				return FilePermissionError{error: err}
			}

			return LocalFileError{error: err}

		default:
			return err
		}
	}

	return nil
}

// walks through the local files
func walkLocalFiles(sources []string, cb LocalWalkCb) (totalFiles, totalDirectories, totalSize int64, err error) {
	totalFiles = 0
	totalDirectories = 0
	totalSize = 0

	for _, source := range sources {
		// walk through the source
		err := filepath.Walk(source,
			func(path string, fInfo os.FileInfo, err error) error {
				if err != nil {
					return err
				}

				name := fInfo.Name()

				// don't follow symlinks
				if isSymlinkLocal(fInfo) {
					return nil
				}

				// filter out disallowed files
				if isDisallowedFiles(name) {
					return nil
				}

				if err := cb(&fInfo, nil); err != nil {
					return err
				}

				if !fInfo.IsDir() {
					totalFiles += 1
					totalSize += fInfo.Size()
				} else {
					totalDirectories += 1
				}

				return nil
			})

		if err != nil {
			switch err.(type) {
			case *os.PathError:
				if errors.Is(err, os.ErrPermission) {
					return totalFiles, totalDirectories, totalSize, FilePermissionError{error: err}
				}

				return totalFiles, totalDirectories, totalSize, LocalFileError{error: err}

			default:
				return totalFiles, totalDirectories, totalSize, err
			}
		}
	}

	return totalFiles, totalDirectories, totalSize, nil
}
