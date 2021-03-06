package state

import (
	"bytes"
	"fmt"
	"io"

	"github.com/hyperledger/burrow/encoding"
	"github.com/hyperledger/burrow/execution/exec"
)

func (ws *writeState) AddBlock(be *exec.BlockExecution) error {
	// If there are no transactions, do not store anything. This reduces the amount of data we store and
	// prevents the iavl tree from changing, which means the AppHash does not change.
	if len(be.TxExecutions) == 0 {
		return nil
	}

	buf := new(bytes.Buffer)
	var offset int
	for _, ev := range be.StreamEvents() {
		if ev.BeginTx != nil {
			val := &exec.TxExecutionKey{Height: be.Height, Offset: uint64(offset)}
			bs, err := encoding.Encode(val)
			if err != nil {
				return err
			}
			// Set reference to TxExecution
			ws.plain.Set(keys.TxHash.Key(ev.BeginTx.TxHeader.TxHash), bs)
		}

		n, err := encoding.WriteMessage(buf, ev)
		if err != nil {
			return err
		}
		offset += n
	}

	tree, err := ws.forest.Writer(keys.Event.Prefix())
	if err != nil {
		return err
	}
	key := keys.Event.KeyNoPrefix(be.Height)
	tree.Set(key, buf.Bytes())

	return nil
}

// Iterate SteamEvents over the closed interval [startHeight, endHeight] - i.e. startHeight and endHeight inclusive
func (s *ReadState) IterateStreamEvents(startHeight, endHeight *uint64, consumer func(*exec.StreamEvent) error) error {
	tree, err := s.Forest.Reader(keys.Event.Prefix())
	if err != nil {
		return err
	}
	var startKey, endKey []byte
	if startHeight != nil {
		startKey = keys.Event.KeyNoPrefix(*startHeight)
	}
	if endHeight != nil {
		// Convert to inclusive end bounds since this generally makes more sense for block height
		endKey = keys.Event.KeyNoPrefix(*endHeight + 1)
	}
	return tree.Iterate(startKey, endKey, true, func(_, value []byte) error {
		buf := bytes.NewBuffer(value)

		for {
			ev := new(exec.StreamEvent)
			_, err := encoding.ReadMessage(buf, ev)
			if err != nil {
				if err == io.EOF {
					return nil
				}
				return err
			}

			err = consumer(ev)
			if err != nil {
				return err
			}
		}
	})
}

func (s *ReadState) TxsAtHeight(height uint64) ([]*exec.TxExecution, error) {
	var stack exec.TxStack
	var txExecutions []*exec.TxExecution
	err := s.IterateStreamEvents(&height, &height,
		func(ev *exec.StreamEvent) error {
			// Keep trying to consume TxExecutions at from events at this height
			txe := stack.Consume(ev)
			if txe != nil {
				txExecutions = append(txExecutions, txe)
			}
			return nil
		})
	if err != nil && err != io.EOF {
		return nil, err
	}
	return txExecutions, nil
}

func (s *ReadState) TxByHash(txHash []byte) (*exec.TxExecution, error) {
	const errHeader = "TxByHash():"
	bs := s.Plain.Get(keys.TxHash.Key(txHash))
	if len(bs) == 0 {
		return nil, nil
	}

	key := new(exec.TxExecutionKey)
	err := encoding.Decode(bs, key)
	if err != nil {
		return nil, err
	}

	blockTree, err := s.Forest.Reader(keys.Event.Prefix())
	if err != nil {
		return nil, err
	}

	bs = blockTree.Get(keys.Event.KeyNoPrefix(key.Height))
	if len(bs) == 0 {
		return nil, fmt.Errorf("%s could not retrieve transaction with TxHash %X despite finding reference",
			errHeader, txHash)
	}

	buf := bytes.NewBuffer(bs[key.Offset:])
	var stack exec.TxStack

	for {
		ev := new(exec.StreamEvent)
		_, err := encoding.ReadMessage(buf, ev)
		if err != nil {
			return nil, err
		}

		txe := stack.Consume(ev)
		if txe != nil {
			return txe, nil
		}
	}
}
