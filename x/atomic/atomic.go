// Copyright 2016, Google
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package atomic implements an experimental interface for using B2 as a
// coordination primitive.
package atomic

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"

	"github.com/kurin/blazer/b2"
)

const metaKey = "blazer-meta-key-no-touchie"

var (
	errUpdateConflict = errors.New("update conflict")
	errNotInGroup     = errors.New("not in group")
)

// NewGroup creates a new atomic Group for the given bucket.
func NewGroup(bucket *b2.Bucket, name string) *Group {
	return &Group{
		name: name,
		b:    bucket,
	}
}

// Group represents a collection of B2 objects that can be modified atomically.
// Objects in the same group contend with each other for updates, but there can
// only be so many (maximum of 10; fewer if there are other bucket attributes
// set) groups in a given bucket.
type Group struct {
	name string
	b    *b2.Bucket
	ba   *b2.BucketAttrs
}

// TODO: consider OperateStream(ctx context.Context, name string, f func(io.Reader) (io.Reader, error)

// Operate calls f with the contents of the group object given by name, and
// updates that object with the output of f if f returns no error.  Operate
// guarantees that no other callers have modified the contents of name in the
// meantime (as long as all other callers are using this package).  It may call
// f any number of times.
func (g *Group) Operate(ctx context.Context, name string, f func([]byte) ([]byte, error)) error {
	for {
		var b []byte
		r, err := g.NewReader(ctx, name)
		if err != nil {
			if err == errNotInGroup {
				goto call
			}
			return err
		}
		b, err = ioutil.ReadAll(r)
		r.Close()
		if b2.IsNotExist(err) {
			goto call
		}
		if err != nil {
			return err
		}
	call:
		o, err := f(b)
		if err != nil {
			return err
		}
		w, err := g.NewWriter(ctx, r.Key, name)
		if err != nil {
			return err
		}
		if _, err := io.Copy(w, bytes.NewReader(o)); err != nil {
			return err
		}
		if err := w.Close(); err != nil {
			if err == errUpdateConflict {
				continue
			}
			return err
		}
		return nil
	}
}

// Writer is an io.ReadCloser.
type Writer struct {
	ctx    context.Context
	wc     io.WriteCloser
	name   string
	suffix string
	key    string
	g      *Group
}

// Write implements io.Write.
func (w Writer) Write(p []byte) (int, error) { return w.wc.Write(p) }

// Close writes any remaining data into B2 and updates the group to reflect the
// contents of the new object.  If the group object has been modified, Close()
// will fail.
func (w Writer) Close() error {
	if err := w.wc.Close(); err != nil {
		return err
	}
	// TODO: maybe see if you can cut down on calls to info()
	for {
		ai, err := w.g.info(w.ctx)
		if err != nil {
			// Replacement failed; delete the new version.
			w.g.b.Object(w.name + "/" + w.suffix).Delete(w.ctx)
			return err
		}
		old, ok := ai.Locations[w.name]
		if ok && old != w.key {
			w.g.b.Object(w.name + "/" + w.suffix).Delete(w.ctx)
			return errUpdateConflict
		}
		ai.Locations[w.name] = w.suffix
		if err := w.g.save(w.ctx, ai); err != nil {
			if err == errUpdateConflict {
				continue
			}
			w.g.b.Object(w.name + "/" + w.suffix).Delete(w.ctx)
			return err
		}
		// Replacement successful; delete the old version.
		w.g.b.Object(w.name + "/" + w.key).Delete(w.ctx)
		return nil
	}
}

// Reader is an io.ReadCloser.  Key must be passed to NewWriter.
type Reader struct {
	io.ReadCloser
	Key string
}

// NewWriter creates a Writer and prepares it to be updated.  The key argument
// should come from the Key field of a Reader; if Writer.Close() returns with
// no error, then the underlying group object was successfully updated from the
// data available from the Reader with no intervening writes.  New objects can
// be created with an empty key.
func (g *Group) NewWriter(ctx context.Context, key, name string) (Writer, error) {
	suffix, err := random()
	if err != nil {
		return Writer{}, err
	}
	return Writer{
		ctx:    ctx,
		wc:     g.b.Object(name + "/" + suffix).NewWriter(ctx),
		name:   name,
		suffix: suffix,
		key:    key,
		g:      g,
	}, nil
}

// NewReader creates a Reader with the current version of the object, as well
// as that object's update key.
func (g *Group) NewReader(ctx context.Context, name string) (Reader, error) {
	ai, err := g.info(ctx)
	if err != nil {
		return Reader{}, err
	}
	suffix, ok := ai.Locations[name]
	if !ok {
		return Reader{}, errNotInGroup
	}
	return Reader{
		ReadCloser: g.b.Object(name + "/" + suffix).NewReader(ctx),
		Key:        suffix,
	}, nil
}

func (g *Group) info(ctx context.Context) (*atomicInfo, error) {
	attrs, err := g.b.Attrs(ctx)
	if err != nil {
		return nil, err
	}
	g.ba = attrs
	imap := attrs.Info
	if imap == nil {
		return nil, nil
	}
	enc, ok := imap[metaKey+"-"+g.name]
	if !ok {
		return &atomicInfo{
			Version:   1,
			Locations: make(map[string]string),
		}, nil
	}
	b, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return nil, err
	}
	ai := &atomicInfo{}
	if err := json.Unmarshal(b, ai); err != nil {
		return nil, err
	}
	if ai.Locations == nil {
		ai.Locations = make(map[string]string)
	}
	return ai, nil
}

func (g *Group) save(ctx context.Context, ai *atomicInfo) error {
	ai.Serial++
	b, err := json.Marshal(ai)
	if err != nil {
		return err
	}
	s := base64.StdEncoding.EncodeToString(b)

	for {
		oldAI, err := g.info(ctx)
		if err != nil {
			return err
		}
		if oldAI.Serial != ai.Serial-1 {
			return errUpdateConflict
		}
		if g.ba.Info == nil {
			g.ba.Info = make(map[string]string)
		}
		g.ba.Info[metaKey+"-"+g.name] = s
		err = g.b.Update(ctx, g.ba)
		if err == nil {
			return nil
		}
		if !b2.IsUpdateConflict(err) {
			return err
		}
		// Bucket update conflict; try again.
	}
}

// List returns a list of all the group objects.
func (g *Group) List(ctx context.Context) ([]string, error) {
	ai, err := g.info(ctx)
	if err != nil {
		return nil, err
	}
	var l []string
	for name := range ai.Locations {
		l = append(l, name)
	}
	return l, nil
}

type atomicInfo struct {
	Version int

	// Serial is incremented for every version saved.  If we ensure that
	// current.Serial = 1 + previous.Serial, and that the bucket metadata is
	// updated cleanly, then we know that the version we saved is the direct
	// successor to the version we had.  If the bucket metadata doesn't update
	// cleanly, but the serial relation holds true for the new AI struct, then we
	// can retry without bothering the user.  However, if the serial relation no
	// longer holds true, it means someone else has updated AI and we have to ask
	// the user to redo everything they've done.
	//
	// However, it is still necessary for higher level constructs to confirm that
	// the serial number they expect is good.  The writer does this, for example,
	// but comparing the "key" of the file it is replacing.
	Serial    int
	Locations map[string]string
}

func random() (string, error) {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", b), nil
}