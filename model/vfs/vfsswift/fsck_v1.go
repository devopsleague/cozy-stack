package vfsswift

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"path"
	"strings"

	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/ncw/swift/v2"
)

func (sfs *swiftVFS) Fsck(accumulate func(log *vfs.FsckLog), failFast bool) error {
	entries := make(map[string]*vfs.TreeFile, 1024)
	tree, err := sfs.BuildTree(func(f *vfs.TreeFile) {
		if !f.IsOrphan && f.DocID != consts.RootDirID && f.DocID != consts.TrashDirID {
			entries[f.DirID+"/"+f.DocName] = f
		}
	})
	if err != nil {
		return err
	}
	if err = sfs.CheckTreeIntegrity(tree, accumulate, failFast); err != nil {
		if errors.Is(err, vfs.ErrFsckFailFast) {
			return nil
		}
		return err
	}
	return sfs.checkFiles(entries, accumulate, failFast)
}

func (sfs *swiftVFS) CheckFilesConsistency(accumulate func(log *vfs.FsckLog), failFast bool) error {
	entries := make(map[string]*vfs.TreeFile, 1024)
	_, err := sfs.BuildTree(func(f *vfs.TreeFile) {
		if !f.IsOrphan && f.DocID != consts.RootDirID && f.DocID != consts.TrashDirID {
			entries[f.DirID+"/"+f.DocName] = f
		}
	})
	if err != nil {
		return err
	}
	return sfs.checkFiles(entries, accumulate, failFast)
}

func (sfs *swiftVFS) checkFiles(
	entries map[string]*vfs.TreeFile,
	accumulate func(log *vfs.FsckLog),
	failFast bool,
) error {
	var orphansObjs []swift.Object

	opts := &swift.ObjectsOpts{Limit: 10_000}
	err := sfs.c.ObjectsWalk(sfs.ctx, sfs.container, opts, func(ctx context.Context, opts *swift.ObjectsOpts) (interface{}, error) {
		objs, err := sfs.c.Objects(sfs.ctx, sfs.container, opts)
		if err != nil {
			return nil, err
		}
		for _, obj := range objs {
			f, ok := entries[obj.Name]
			if !ok {
				if failFast {
					if obj.ContentType == dirContentType {
						accumulate(&vfs.FsckLog{
							Type:   vfs.IndexMissing,
							IsFile: false,
							DirDoc: objectToFileDocV1(sfs.container, obj),
						})
					} else {
						accumulate(&vfs.FsckLog{
							Type:    vfs.IndexMissing,
							IsFile:  true,
							FileDoc: objectToFileDocV1(sfs.container, obj),
						})
					}
					return nil, errFailFast
				}
				orphansObjs = append(orphansObjs, obj)
			} else if f.IsDir != (obj.ContentType == dirContentType) {
				if f.IsDir {
					accumulate(&vfs.FsckLog{
						Type:    vfs.TypeMismatch,
						IsFile:  true,
						FileDoc: objectToFileDocV1(sfs.container, obj),
						DirDoc:  f,
					})
				} else {
					accumulate(&vfs.FsckLog{
						Type:    vfs.TypeMismatch,
						IsFile:  false,
						DirDoc:  f,
						FileDoc: objectToFileDocV1(sfs.container, obj),
					})
				}
				if failFast {
					return nil, errFailFast
				}
			} else if !f.IsDir {
				var md5sum []byte
				md5sum, err = hex.DecodeString(obj.Hash)
				if err != nil {
					return nil, err
				}
				if !bytes.Equal(md5sum, f.MD5Sum) || f.ByteSize != obj.Bytes {
					accumulate(&vfs.FsckLog{
						Type:    vfs.ContentMismatch,
						IsFile:  true,
						FileDoc: f,
						ContentMismatch: &vfs.FsckContentMismatch{
							SizeFile:    obj.Bytes,
							SizeIndex:   f.ByteSize,
							MD5SumFile:  md5sum,
							MD5SumIndex: f.MD5Sum,
						},
					})
					if failFast {
						return nil, errFailFast
					}
				}
			}
			delete(entries, obj.Name)
		}
		return objs, err
	})
	if err != nil {
		if errors.Is(err, errFailFast) {
			return nil
		}
		return err
	}

	for _, f := range entries {
		if f.IsDir {
			accumulate(&vfs.FsckLog{
				Type:   vfs.FSMissing,
				IsFile: false,
				DirDoc: f,
			})
		} else {
			accumulate(&vfs.FsckLog{
				Type:    vfs.FSMissing,
				IsFile:  true,
				FileDoc: f,
			})
		}
		if failFast {
			return nil
		}
	}

	for _, obj := range orphansObjs {
		if obj.ContentType == dirContentType {
			accumulate(&vfs.FsckLog{
				Type:   vfs.IndexMissing,
				IsFile: false,
				DirDoc: objectToFileDocV1(sfs.container, obj),
			})
		} else {
			accumulate(&vfs.FsckLog{
				Type:    vfs.IndexMissing,
				IsFile:  true,
				FileDoc: objectToFileDocV1(sfs.container, obj),
			})
		}
		if failFast {
			return nil
		}
	}

	return nil
}

func objectToFileDocV1(container string, object swift.Object) *vfs.TreeFile {
	var dirID, name string
	if dirIDAndName := strings.SplitN(object.Name, "/", 2); len(dirIDAndName) == 2 {
		dirID = dirIDAndName[0]
		name = dirIDAndName[1]
	}
	docType := consts.FileType
	if object.ContentType == dirContentType {
		docType = consts.DirType
	}
	md5sum, _ := hex.DecodeString(object.Hash)
	mime, class := vfs.ExtractMimeAndClass(object.ContentType)
	return &vfs.TreeFile{
		DirOrFileDoc: vfs.DirOrFileDoc{
			DirDoc: &vfs.DirDoc{
				Type:      docType,
				DocName:   name,
				DirID:     dirID,
				CreatedAt: object.LastModified,
				UpdatedAt: object.LastModified,
				Fullpath:  path.Join(vfs.OrphansDirName, name),
			},
			ByteSize:   object.Bytes,
			Mime:       mime,
			Class:      class,
			Executable: false,
			MD5Sum:     md5sum,
		},
	}
}
