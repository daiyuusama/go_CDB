package CaskDB

import (
	"bytes"
	"github.com/k-si/CaskDB/util"
	"io/ioutil"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

func (db *DB) listeningGC() {
	timer := time.NewTimer(db.config.MergeInterval)
	defer func() {
		if !timer.Stop() {
			<-timer.C
		}
	}()

Over:
	for {
		select {
		case <-db.listenChan:
			break Over
		case <-timer.C:
			timer.Reset(db.config.MergeInterval)
			if err := db.GC(); err != nil {
				log.Println("[GC err]", err)

				// file rollback
				if err := db.FilesRollback(); err != nil {
					log.Fatal("[rollback fail]")
				}

				// fd rollback
				for _, f := range db.activeFiles {
					if err := f.Close(true); err != nil {
						log.Fatal(err)
					}
				}
				for _, item := range db.archedFiles {
					for _, f := range item {
						if err := f.Close(true); err != nil {
							log.Fatal(err)
						}
					}
				}
				db.activeFiles, db.archedFiles, _, err = db.loadFiles()
				if err != nil {
					log.Fatal(err)
				}
				log.Println("[rollback finish]")
			}
			log.Println(">>> restart the world <<<")
		}
	}
}

// garbage file recycling
// merge all files and remove useless data
func (db *DB) GC() error {

	// check status
	if atomic.LoadUint32(&db.isClosed) == 1 {
		return ErrorClosedDB
	}
	if atomic.LoadUint32(&db.isMerging) == 1 {
		return ErrorMergingMerge
	}

	log.Println(">>> stop the world <<<")
	db.strIndex.mu.Lock()
	db.hashIndex.mu.Lock()
	db.listIndex.mu.Lock()
	db.setIndex.mu.Lock()
	db.zsetIndex.mu.Lock()
	defer db.strIndex.mu.Unlock()
	defer db.hashIndex.mu.Unlock()
	defer db.listIndex.mu.Unlock()
	defer db.setIndex.mu.Unlock()
	defer db.zsetIndex.mu.Unlock()

	// change status
	atomic.StoreUint32(&db.isMerging, 1)
	defer atomic.StoreUint32(&db.isMerging, 0)

	// backup
	if err := db.FilesBackup(); err != nil {
		return err
	}

	// check merged path
	mergePath := db.config.DBDir + PathSeparator + MergeDirName
	if err := util.CheckAndMakeDir(mergePath); err != nil {
		return err
	}

	// all files that fail to merge will be deleted
	if err := db.removeMergedFiles(); err != nil {
		return err
	}

	// load all files id
	fids, err := loadFilesId(db.config.DBDir)
	if err != nil {
		return err
	}

	var mergeErr error

	wg := sync.WaitGroup{}
	for i := 0; i < DataTypeNum; i++ {
		wg.Add(1)

		// every goroutine do its merge task
		go func(i int) {
			defer wg.Done()

			// List snapshot GC
			if i == 1 {
				log.Println("[list snapshot...]")
				if mergeErr = db.listSnapshot(mergePath); mergeErr != nil {
					return
				}
				return
			}

			ids := fids[i] // all files with this type

			mergedArchedFiles := make(map[uint32]*File)
			var mergedActiveFile *File

			for j := 0; j < len(ids); j++ {
				select {
				case <-db.mergeChan:
					log.Println("[exit merge task]", i)
					return
				default:
					f, err := db.getFileById(uint16(i), uint32(ids[j]))
					if err != nil {
						return
					}

					// read all entry from files, but except empty file
					var offset int64
					if 0 == f.offset {
						continue
					}
					for offset < f.offset {
						e, err := f.Read(offset)
						if err != nil {
							mergeErr = err
							return
						}

						// check entry valid
						if ok := db.entryValid(e, uint32(ids[j]), offset); ok {

							// store entry
							// here use &, make mergedActiveFile modifiable
							if mergeErr = db.storeMerged(e, mergedArchedFiles, &mergedActiveFile); mergeErr != nil {
								return
							}
						}
						offset += int64(e.Size())
					}

					// close and remove old file
					if mergeErr = f.Close(true); mergeErr != nil {
						return
					}
					if mergeErr = os.Remove(f.fd.Name()); mergeErr != nil {
						return
					}
				}
			}

			if mergeErr = db.buildFromMerged(mergedActiveFile, mergedArchedFiles, i, mergePath); mergeErr != nil {
				return
			}

		}(i)
	}
	wg.Wait()

	if mergeErr != nil {
		return mergeErr
	}

	return nil
}

func (db *DB) StopGC() error {

	// check status
	if atomic.LoadUint32(&db.isClosed) == 1 {
		return ErrorClosedDB
	}

	if atomic.LoadUint32(&db.isMerging) == 0 {
		db.listenChan <- struct{}{}
		return nil
	}

	// channel notify
	if atomic.LoadUint32(&db.isMerging) == 1 {
		go func() {
			for i := 0; i < DataTypeNum; i++ {
				db.mergeChan <- struct{}{}
			}
		}()
	}
	return nil
}

func (db *DB) buildFromMerged(mergedActiveFile *File, mergedArchedFiles map[uint32]*File, i int, mergePath string) error {
	if mergedActiveFile != nil {

		// save file info
		fi, err := mergedActiveFile.fd.Stat()
		if err != nil {
			return err
		}
		name := PathSeparator + fi.Name()
		tmpId := mergedActiveFile.id
		tmpOff := mergedActiveFile.offset

		// close merged file
		if err = mergedActiveFile.Close(true); err != nil {
			return err
		}

		// rename new merged file
		oldPath := mergePath + name
		newPath := db.config.DBDir + name
		if err = os.Rename(oldPath, newPath); err != nil {
			return err
		}

		// reopen file
		activeFile, err := NewFile(db.config.DBDir, tmpId, uint16(i), db.config.MaxFileSize)
		if err != nil {
			return err
		}
		activeFile.offset = tmpOff

		// do same
		var archedFiles = make(map[uint32]*File)
		for _, f := range mergedArchedFiles {

			// save info
			fi, err = f.fd.Stat()
			if err != nil {
				return err
			}
			name = PathSeparator + fi.Name()
			tmpId = f.id
			tmpOff = f.offset

			// close merged file
			if err = f.Close(true); err != nil {
				return err
			}

			// rename file
			oldPath = mergePath + name
			newPath = db.config.DBDir + name
			if err = os.Rename(oldPath, newPath); err != nil {
				return err
			}

			// reopen file
			ac, err := NewFile(db.config.DBDir, tmpId, uint16(i), db.config.MaxFileSize)
			if err != nil {
				return err
			}
			ac.offset = tmpOff
			archedFiles[ac.id] = ac
		}

		// update fd
		db.activeFiles[i] = activeFile
		db.archedFiles[i] = archedFiles
	}

	return nil
}

// clear the files in merged directory
func (db *DB) removeMergedFiles() error {
	mergedPath := db.config.DBDir + PathSeparator + MergeDirName
	mInfos, err := ioutil.ReadDir(mergedPath)
	if err != nil {
		return err
	}
	for _, mi := range mInfos {
		fp := mergedPath + PathSeparator + mi.Name()
		if err := os.Remove(fp); err != nil {
			return err
		}
	}
	return nil
}

// while merging, need 'set' or 'update' type of entry, 'remove' type is useless
func (db *DB) entryValid(e *Entry, eFid uint32, eOffset int64) bool {
	if e == nil {
		return false
	}

	dt := e.GetDataType()
	mt := e.GetMarkType()
	switch dt {
	case Str:
		if mt == StrSet {

			// entry is valid, if key, file id, offset all equals index
			v := db.strIndex.idx.Get(e.key)
			if v == nil {
				return false
			}
			idx := v.(*Index)
			if eFid == idx.fileId && eOffset == idx.offset {
				return true
			}
			return false
		}
	case List:
		// unable to determine whether List entry is valid,
		// because if we push 'a' and pop 'a' and push 'a',
		// when GC, we can not ensure the correctness of 'a',
		// the solution is to use snapshots.
	case Hash:
		if mt == HashHSet {
			v := db.hashIndex.idx.Get(e.GetPreKey(), e.GetPostKey())
			if v != nil {
				return bytes.Compare(e.value, v) == 0
			}
		}
	case Set:
		if mt == SetSAdd {
			if db.setIndex.idx.ValExist(string(e.key), string(e.value)) {
				return true
			}
		} else if mt == SetSMove {
			if db.setIndex.idx.ValExist(e.GetPostKey(), string(e.value)) {
				return true
			}
		}
	case ZSet:
		if mt == ZSetZAdd {
			score1 := util.BytesToFloat64(e.GetPostBytesKey())
			ok, score2 := db.zsetIndex.idx.GetScore(e.GetPreKey(), string(e.value))
			if ok && score1 == score2 {
				return true
			}
		}
	}

	return false
}

// store entry in merged files
// we should use *activeFile
func (db *DB) storeMerged(e *Entry, archedFiles map[uint32]*File, activeFile **File) error {
	mergePath := db.config.DBDir + PathSeparator + MergeDirName

	// init
	if (*activeFile) == nil {
		f, err := NewFile(mergePath, 0, e.GetDataType(), db.config.MaxFileSize)
		if err != nil {
			return err
		}
		*activeFile = f
	}

	// check active file size
	if (*activeFile).offset+int64(e.Size()) > db.config.MaxFileSize {

		// flush current active file to disk
		if err := (*activeFile).Sync(); err != nil {
			return err
		}

		// create new file as active file
		newId := (*activeFile).id + 1
		newf, err := NewFile(mergePath, newId, e.GetDataType(), db.config.MaxFileSize)
		if err != nil {
			return err
		}
		archedFiles[(*activeFile).id] = *activeFile
		*activeFile = newf
	}

	// write entry in active file
	if err := (*activeFile).Write(e); err != nil {
		return err
	}

	// update str indexes. list, hash, set, zset exist in memory,
	// they dont have to update index
	switch e.GetDataType() {
	case Str:
		idx := db.strIndex.idx.Get(e.key).(*Index)
		idx.fileId = (*activeFile).id
		idx.offset = (*activeFile).offset - int64(e.Size())
	}

	// sync buffer with disk
	if err := (*activeFile).Sync(); err != nil {
		return err
	}

	return nil
}
