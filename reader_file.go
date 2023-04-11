package squashfs

import (
	"errors"
	"io"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/CalebQ42/squashfs/internal/data"
	"github.com/CalebQ42/squashfs/internal/directory"
	"github.com/CalebQ42/squashfs/internal/inode"
)

// File represents a file inside a squashfs archive.
type File struct {
	i        inode.Inode
	rdr      io.Reader
	fullRdr  *data.FullReader
	r        *Reader
	parent   *FS
	e        directory.Entry
	dirsRead int
}

var (
	ErrReadNotFile = errors.New("read called on non-file")
)

func (r Reader) newFile(en directory.Entry, parent *FS) (*File, error) {
	i, err := r.inodeFromDir(en)
	if err != nil {
		return nil, err
	}
	var rdr io.Reader
	var full *data.FullReader
	if i.Type == inode.Fil || i.Type == inode.EFil {
		full, rdr, err = r.getReaders(i)
		if err != nil {
			return nil, err
		}
	}
	return &File{
		e:       en,
		i:       i,
		rdr:     rdr,
		fullRdr: full,
		r:       &r,
		parent:  parent,
	}, nil
}

// Stat returns the File's fs.FileInfo
func (f File) Stat() (fs.FileInfo, error) {
	return newFileInfo(f.e, f.i), nil
}

// Read reads the data from the file. Only works if file is a normal file.
func (f File) Read(p []byte) (int, error) {
	if f.i.Type != inode.Fil && f.i.Type != inode.EFil {
		return 0, ErrReadNotFile
	}
	if f.rdr == nil {
		return 0, fs.ErrClosed
	}
	return f.rdr.Read(p)
}

func (f File) ReadAt(p []byte, off int64) (int, error) {
	if f.i.Type != inode.Fil && f.i.Type != inode.EFil {
		return 0, ErrReadNotFile
	}
	return f.fullRdr.ReadAt(p, off)
}

// WriteTo writes all data from the file to the writer. This is multi-threaded.
// The underlying reader is seperate from the one used with Read and can be reused.
func (f File) WriteTo(w io.Writer) (int64, error) {
	if f.i.Type != inode.Fil && f.i.Type != inode.EFil {
		return 0, ErrReadNotFile
	}
	return f.fullRdr.WriteTo(w)
}

// Close simply nils the underlying reader. Here mostly to satisfy fs.File
func (f *File) Close() error {
	f.rdr = nil
	return nil
}

// ReadDir returns n fs.DirEntry's that's contained in the File (if it's a directory).
// If n <= 0 all fs.DirEntry's are returned.
func (f *File) ReadDir(n int) (out []fs.DirEntry, err error) {
	if !f.IsDir() {
		return nil, errors.New("file is not a directory")
	}
	ents, err := f.r.readDirectory(f.i)
	if err != nil {
		return nil, err
	}
	start, end := 0, len(ents)
	if n > 0 {
		start, end = f.dirsRead, f.dirsRead+n
		if end > len(f.r.e) {
			end = len(f.r.e)
			err = io.EOF
		}
	}
	var fi fileInfo
	for _, e := range ents[start:end] {
		fi, err = f.r.newFileInfo(e)
		if err != nil {
			f.dirsRead += len(out)
			return
		}
		out = append(out, fs.FileInfoToDirEntry(fi))
	}
	f.dirsRead += len(out)
	return
}

// FS returns the File as a FS.
func (f *File) FS() (*FS, error) {
	if !f.IsDir() {
		return nil, errors.New("File is not a directory")
	}
	ents, err := f.r.readDirectory(f.i)
	if err != nil {
		return nil, err
	}
	return &FS{
		File: f,
		e:    ents,
	}, nil
}

// IsDir Yep.
func (f File) IsDir() bool {
	return f.i.Type == inode.Dir || f.i.Type == inode.EDir
}

// IsRegular yep.
func (f File) IsRegular() bool {
	return f.i.Type == inode.Fil || f.i.Type == inode.EFil
}

// IsSymlink yep.
func (f File) IsSymlink() bool {
	return f.i.Type == inode.Sym || f.i.Type == inode.ESym
}

func (f File) isDeviceOrFifo() bool {
	return f.i.Type == inode.Char || f.i.Type == inode.Block || f.i.Type == inode.EChar || f.i.Type == inode.EBlock || f.i.Type == inode.Fifo || f.i.Type == inode.EFifo
}

func (f File) deviceDevices() (maj uint32, min uint32) {
	var dev uint32
	if f.i.Type == inode.Char || f.i.Type == inode.Block {
		dev = f.i.Data.(inode.Device).Dev
	} else if f.i.Type == inode.EChar || f.i.Type == inode.EBlock {
		dev = f.i.Data.(inode.EDevice).Dev
	}
	return dev >> 8, dev & 0x000FF
}

// SymlinkPath returns the symlink's target path. Is the File isn't a symlink, returns an empty string.
func (f File) SymlinkPath() string {
	switch f.i.Type {
	case inode.Sym:
		return string(f.i.Data.(inode.Symlink).Target)
	case inode.ESym:
		return string(f.i.Data.(inode.ESymlink).Target)
	}
	return ""
}

func (f File) path() string {
	if f.parent == nil {
		return f.e.Name
	}
	return f.parent.path() + "/" + f.e.Name
}

// GetSymlinkFile returns the File the symlink is pointing to.
// If not a symlink, or the target is unobtainable (such as it being outside the archive or it's absolute) returns nil
func (f File) GetSymlinkFile() *File {
	if !f.IsSymlink() {
		return nil
	}
	if strings.HasPrefix(f.SymlinkPath(), "/") {
		return nil
	}
	sym, err := f.parent.Open(f.SymlinkPath())
	if err != nil {
		return nil
	}
	return sym.(*File)
}

// ExtractionOptions are available options on how to extract.
type ExtractionOptions struct {
	LogOutput          io.Writer   //Where error log should write. If nil, uses os.Stdout. Has no effect if verbose is false.
	DereferenceSymlink bool        //Replace symlinks with the target file
	UnbreakSymlink     bool        //Try to make sure symlinks remain unbroken when extracted, without changing the symlink
	Verbose            bool        //Prints extra info to log on an error
	FolderPerm         fs.FileMode //The permissions used when creating the extraction folder
}

// DefaultOptions is the default ExtractionOptions.
func DefaultOptions() ExtractionOptions {
	return ExtractionOptions{
		FolderPerm: 0755,
	}
}

// ExtractTo extracts the File to the given folder with the default options.
// If the File is a directory, it instead extracts the directory's contents to the folder.
func (f File) ExtractTo(folder string) error {
	return f.ExtractWithOptions(folder, DefaultOptions())
}

// ExtractSymlink extracts the File to the folder with the DereferenceSymlink option.
// If the File is a directory, it instead extracts the directory's contents to the folder.
func (f File) ExtractSymlink(folder string) error {
	return f.ExtractWithOptions(folder, ExtractionOptions{
		DereferenceSymlink: true,
		FolderPerm:         0755,
	})
}

// ExtractWithOptions extracts the File to the given folder with the given ExtrationOptions.
// If the File is a directory, it instead extracts the directory's contents to the folder.
func (f File) ExtractWithOptions(folder string, op ExtractionOptions) error {
	if op.Verbose {
		if op.LogOutput == nil {
			op.LogOutput = os.Stdout
		}
		log.SetOutput(op.LogOutput)
	}
	return f.realExtract(folder, op)
}

func (f File) realExtract(folder string, op ExtractionOptions) error {
	err := os.MkdirAll(folder, op.FolderPerm)
	folder = filepath.Clean(folder)
	if err != nil && !os.IsExist(err) {
		if op.Verbose {
			log.Println("Error while creating extraction folder")
		}
		return err
	}
	switch {
	case f.IsDir():
		filFS, _ := f.FS()
		var ents []directory.Entry
		ents, err = f.r.readDirectory(f.i)
		if err != nil {
			if op.Verbose {
				log.Println("Error while reading children of", f.path())
			}
			return err
		}
		errChan := make(chan error)
		for i := 0; i < len(ents); i++ {
			go func(ent directory.Entry) {
				fil, goErr := f.r.newFile(ent, filFS)
				if goErr != nil {
					if op.Verbose {
						log.Println("Error while reading info for", filepath.Join(f.path(), ent.Name))
					}
					errChan <- goErr
					return
				}
				if fil.IsDir() {
					info, _ := fil.Stat()
					err = os.Mkdir(filepath.Join(folder, fil.e.Name), info.Mode())
					if err != nil {
						if op.Verbose {
							log.Println("Error while creating", filepath.Join(folder, fil.e.Name))
						}
						errChan <- err
						return
					}
					errChan <- fil.realExtract(filepath.Join(folder, fil.e.Name), op)
				} else {
					errChan <- fil.realExtract(folder, op)
				}
				fil.Close()
			}(ents[i])
		}
		for i := 0; i < len(ents); i++ {
			err = <-errChan
			if err != nil {
				return err
			}
		}
	case f.IsRegular():
		var fil *os.File
		fil, err = os.Create(folder + "/" + f.e.Name)
		if os.IsExist(err) {
			os.Remove(folder + "/" + f.e.Name)
			fil, err = os.Create(folder + "/" + f.e.Name)
			if err != nil {
				if op.Verbose {
					log.Println("Error while creating", folder+"/"+f.e.Name)
				}
				return err
			}
		} else if err != nil {
			if op.Verbose {
				log.Println("Error while creating", folder+"/"+f.e.Name)
			}
			return err
		}
		_, err = io.Copy(fil, f)
		if err != nil {
			if op.Verbose {
				log.Println("Error while copying data to", folder+"/"+f.e.Name)
			}
			return err
		}
	case f.IsSymlink():
		symPath := f.SymlinkPath()
		if op.DereferenceSymlink {
			fil := f.GetSymlinkFile()
			if fil == nil {
				if op.Verbose {
					log.Println("Symlink path(", symPath, ") is unobtainable:", folder+"/"+f.e.Name)
				}
				return errors.New("cannot get symlink target")
			}
			fil.e.Name = f.e.Name
			err = fil.realExtract(folder, op)
			if err != nil {
				if op.Verbose {
					log.Println("Error while extracting the symlink's file:", folder+"/"+f.e.Name)
				}
				return err
			}
			return nil
		} else if op.UnbreakSymlink {
			fil := f.GetSymlinkFile()
			if fil == nil {
				if op.Verbose {
					log.Println("Symlink path(", symPath, ") is unobtainable:", folder+"/"+f.e.Name)
				}
				return errors.New("cannot get symlink target")
			}
			extractLoc := filepath.Clean(folder + "/" + filepath.Dir(symPath))
			err = fil.realExtract(extractLoc, op)
			if err != nil {
				if op.Verbose {
					log.Println("Error while extracting ", folder+"/"+f.e.Name)
				}
				return err
			}
		}
		err = os.Symlink(f.SymlinkPath(), folder+"/"+f.e.Name)
		if os.IsExist(err) {
			os.Remove(folder + "/" + f.e.Name)
			err = os.Symlink(f.SymlinkPath(), folder+"/"+f.e.Name)
		}
		if err != nil {
			if op.Verbose {
				log.Println("Error while making symlink:", folder+"/"+f.e.Name)
			}
			return err
		}
	case f.isDeviceOrFifo():
		_, err = exec.LookPath("mknod")
		if err != nil {
			if op.Verbose {
				log.Println("Extracting Fifo IPC or Device and mknod is not in PATH")
			}
			return err
		}
		var typ string
		if f.i.Type == inode.Char || f.i.Type == inode.EChar {
			typ = "c"
		} else if f.i.Type == inode.Block || f.i.Type == inode.EBlock {
			typ = "b"
		} else { //Fifo IPC
			typ = "p"
		}
		cmd := exec.Command("mknod", folder+"/"+f.e.Name, typ)
		if typ != "p" {
			maj, min := f.deviceDevices()
			cmd.Args = append(cmd.Args, strconv.Itoa(int(maj)), strconv.Itoa(int(min)))
		}
		if op.Verbose {
			cmd.Stdout = op.LogOutput
			cmd.Stderr = op.LogOutput
		}
		err = cmd.Run()
		if err != nil {
			if op.Verbose {
				log.Println("Error while running mknod for", folder+"/"+f.e.Name)
			}
			return err
		}
	default:
		return errors.New("Unsupported file type. Inode type: " + strconv.Itoa(int(f.i.Type)))
	}
	return nil
}
