package gcpFS

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"path"
	"time"

	"cloud.google.com/go/storage"
	"github.com/ninjamarcus/ninjaStorage/models"
	"google.golang.org/api/iterator"
)

type GCPFS struct {
	//storage is the gcp storage client
	client *storage.Client
	config *models.GCPFSConfig
	ctx    context.Context
}

type GCPControls interface {
	NewGCPStorage(fs *models.GCPFSConfig) (*GCPFS, error)
	Delete(g *GCPFS, filePath string) error
	Move(g *GCPFS, filePathFrom string, filePathTo string) error
	Copy(g *GCPFS, filePathFrom string, filePathTo string) error
	Find()
	Write(g *GCPFS, data []byte, filePath string, metaData *models.FileMetaData) (*models.FileMetaData, error)
	List(g *GCPFS, prefix string) (map[string]*models.FileMetaData, error)
	Read(g *GCPFS, filePath string) ([]byte, *models.FileMetaData, error)
}

type GCPController struct{}

// NewGCPStorage TO Connect successfully you need to have exported your service account.json file
// as the environment variable GOOGLE_APPLICATION_CREDENTIALS
func (gcp *GCPController) NewGCPStorage(fs *models.GCPFSConfig) (*GCPFS, error) {
	if err := fs.Validate(); err != nil {
		return &GCPFS{}, err
	}
	gcpfs := &GCPFS{config: fs}
	if err := gcpfs.connectToGCPStorage(); err != nil {
		return &GCPFS{}, err
	}

	return gcpfs, nil
}

// Connect to the client
func (g *GCPFS) connectToGCPStorage() error {
	ctx := context.Background()
	client, err := storage.NewClient(ctx)
	if err != nil {
		return err
	}
	g.client = client
	g.ctx = ctx
	defer client.Close()
	return nil
}

func (gcp *GCPController) Delete(g *GCPFS, filePath string) error {
	ctx, cancel := context.WithTimeout(g.ctx, time.Second*10)
	defer cancel()
	fullPath := path.Join(g.config.ParentFolder, filePath)
	o := g.client.Bucket(g.config.BucketName).Object(fullPath)

	attrs, err := o.Attrs(ctx)

	o = o.If(storage.Conditions{GenerationMatch: attrs.Generation})
	if err != nil {
		return fmt.Errorf("object.Attrs: %v", err)
	}
	if err := o.Delete(ctx); err != nil {
		return fmt.Errorf("cannot delete object:%s reason: %v", o.ObjectName(), err)
	}
	return nil

}

func (gcp *GCPController) Move(g *GCPFS, filePathFrom string, filePathTo string) error {
	if err := gcp.Copy(g, filePathFrom, filePathTo); err != nil {
		return fmt.Errorf("could not move/copy file from:%s to:%s reason: %v", filePathFrom, filePathTo, err)
	}
	if err := gcp.Delete(g, filePathFrom); err != nil {
		return fmt.Errorf("could not move/delete file:%s reason: %v", filePathFrom, err)
	}
	return nil
}

func (gcp *GCPController) Copy(g *GCPFS, filePathFrom string, filePathTo string) error {

	if filePathFrom == filePathTo {
		return fmt.Errorf("the filePathFrom: %s, cannot be the same as filePathTo: %s", filePathFrom, filePathTo)
	}
	ctx, cancel := context.WithTimeout(g.ctx, time.Second*10)
	defer cancel()
	from := path.Join(g.config.ParentFolder, filePathFrom)
	to := path.Join(g.config.ParentFolder, filePathTo)

	src := g.client.Bucket(g.config.BucketName).Object(from)
	dst := g.client.Bucket(g.config.BucketName).Object(to)

	dst = dst.If(storage.Conditions{DoesNotExist: true})
	if _, err := dst.CopierFrom(src).Run(ctx); err != nil {
		return fmt.Errorf("Object(%q).CopierFrom(%q).Run: %v", src.ObjectName(), dst.ObjectName(), err)
	}
	return nil
}

func (gcp *GCPController) Find() {
	//TODO implement me
	panic("implement me")
}

func (gcp *GCPController) Write(g *GCPFS, data []byte, filePath string, metaData *models.FileMetaData) (*models.FileMetaData, error) {

	if len(data) == 0 {
		return nil, fmt.Errorf("length of data is 0 nothing to write")
	}
	if filePath == "" {
		return nil, fmt.Errorf("Filepath cannot be empty")
	}

	buf := bytes.NewBuffer(data)
	ctx, cancel := context.WithTimeout(g.ctx, time.Second*50)
	defer cancel()

	fullPath := path.Join(g.config.ParentFolder, filePath)
	o := g.client.Bucket(g.config.BucketName).Object(fullPath)

	wc := o.NewWriter(ctx)
	wc.ChunkSize = 0
	if _, err := io.Copy(wc, buf); err != nil {
		return nil, fmt.Errorf("io.Copy error: %v", err)
	}
	if err := wc.Close(); err != nil {
		return nil, fmt.Errorf("Writer.Close error: %v", err)
	}
	if err := gcp.writeMetadata(g, o, metaData); err != nil {
		return nil, fmt.Errorf("error writing metadata: %v", err)
	}
	attrs, err := o.Attrs(ctx)
	if err != nil {
		return nil, fmt.Errorf("could not retrieve object attributes: %v", err)
	}

	return g.parseMetaData(attrs), nil
}

func (gcp *GCPController) writeMetadata(g *GCPFS, handle *storage.ObjectHandle, metaData *models.FileMetaData) error {

	if len(metaData.UserMetaData) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(g.ctx, time.Second*10)
	defer cancel()
	attrs, err := handle.Attrs(ctx)
	if err != nil {
		return fmt.Errorf("object.Attrs error: %v", err)
	}
	handle = handle.If(storage.Conditions{MetagenerationMatch: attrs.Metageneration})
	objectAttrsToUpdate := storage.ObjectAttrsToUpdate{
		Metadata: metaData.UserMetaData,
	}
	if _, err = handle.Update(ctx, objectAttrsToUpdate); err != nil {
		return fmt.Errorf("ObjectHandle(%q) update failed: %v", handle.ObjectName(), err)
	}
	return nil
}

// List TODO, we might have to disable the with metadata bit for speed but I will remain optimistic.
func (gcp *GCPController) List(g *GCPFS, prefix string) (map[string]*models.FileMetaData, error) {
	results := make(map[string]*models.FileMetaData)
	ctx, cancel := context.WithTimeout(g.ctx, time.Second*10)
	defer cancel()
	fullPath := path.Join(g.config.ParentFolder, prefix)
	it := g.client.Bucket(g.config.BucketName).Objects(ctx, &storage.Query{Prefix: fullPath})

	for {
		attrs, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("Bucket(%s).Objects: %v", g.config.BucketName, err)
		}
		results[attrs.Name] = g.parseMetaData(attrs)
	}
	return results, nil
}

// Take in the metadata/attributes from the file and convert them into a our metadata object.
// TODO: do I need to map this to my own struture or  can I just return googles stuff and somewhere return an interface
// To maintain its generic structure??
func (g *GCPFS) parseMetaData(attrs *storage.ObjectAttrs) *models.FileMetaData {
	return &models.FileMetaData{
		Bucket:       attrs.Bucket,
		Md5Hash:      hex.EncodeToString(attrs.MD5[:]),
		UserMetaData: attrs.Metadata,
		Name:         attrs.Name,
		Size:         attrs.Size,
		TimeCreated:  attrs.Created,
		Updated:      attrs.Updated,
	}
}

func (gcp *GCPController) Read(g *GCPFS, filePath string) ([]byte, *models.FileMetaData, error) {
	ctx, cancel := context.WithTimeout(g.ctx, time.Second*50)
	defer cancel()
	fullPath := path.Join(g.config.ParentFolder, filePath)
	objHandle := g.client.Bucket(g.config.BucketName).Object(fullPath)
	rc, err := objHandle.NewReader(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("object(%s) cannot be read: %v", fullPath, err)
	}
	defer rc.Close()

	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, nil, fmt.Errorf("io.ReadAll failure: %v", err)
	}
	attrs, err := objHandle.Attrs(ctx)
	if err != nil {

	}
	return data, g.parseMetaData(attrs), nil
}
