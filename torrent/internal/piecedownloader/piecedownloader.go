package piecedownloader

import (
	"github.com/cenkalti/rain/torrent/internal/peer"
	"github.com/cenkalti/rain/torrent/internal/peerprotocol"
	"github.com/cenkalti/rain/torrent/internal/piece"
)

// PieceDownloader downloads all blocks of a piece from a peer.
type PieceDownloader struct {
	Piece  *piece.Piece
	Peer   *peer.Peer
	Buffer []byte

	requested      map[uint32]struct{}
	nextBlockIndex uint32
	downloadDone   map[uint32]struct{}
}

type pieceReaderResult struct {
	BlockIndex uint32
	Error      error
}

func New(pi *piece.Piece, pe *peer.Peer, buf []byte) *PieceDownloader {
	return &PieceDownloader{
		Piece:        pi,
		Peer:         pe,
		Buffer:       buf,
		requested:    make(map[uint32]struct{}),
		downloadDone: make(map[uint32]struct{}),
	}
}

func (d *PieceDownloader) Choked() {
	d.requested = make(map[uint32]struct{})
	d.nextBlockIndex = 0
}

func (d *PieceDownloader) GotBlock(block *piece.Block, data []byte) {
	if _, ok := d.downloadDone[block.Index]; ok {
		d.Peer.Logger().Warningln("received duplicate block:", block.Index)
	}
	copy(d.Buffer[block.Begin:block.Begin+block.Length], data)
	delete(d.requested, block.Index)
	d.downloadDone[block.Index] = struct{}{}
}

func (d *PieceDownloader) Rejected(block *piece.Block) {
	delete(d.requested, block.Index)
	d.nextBlockIndex = 0
}

func (d *PieceDownloader) CancelPending() {
	for i := range d.requested {
		b := d.Piece.Blocks[i]
		msg := peerprotocol.CancelMessage{RequestMessage: peerprotocol.RequestMessage{Index: d.Piece.Index, Begin: b.Begin, Length: b.Length}}
		d.Peer.SendMessage(msg)
	}
}

func (d *PieceDownloader) RequestBlocks(queueLength int) {
	for ; d.nextBlockIndex < uint32(len(d.Piece.Blocks)) && len(d.requested) < queueLength; d.nextBlockIndex++ {
		b := d.Piece.Blocks[d.nextBlockIndex]
		if _, ok := d.downloadDone[b.Index]; ok {
			continue
		}
		if _, ok := d.requested[b.Index]; ok {
			continue
		}
		msg := peerprotocol.RequestMessage{Index: d.Piece.Index, Begin: b.Begin, Length: b.Length}
		d.Peer.SendMessage(msg)
		d.requested[b.Index] = struct{}{}
	}
}

func (d *PieceDownloader) Done() bool {
	return len(d.downloadDone) == len(d.Piece.Blocks)
}
