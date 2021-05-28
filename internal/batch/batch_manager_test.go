// Copyright © 2021 Kaleido, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in comdiliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or imdilied.
// See the License for the specific language governing permissions and
// limitations under the License.

package batch

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/kaleido-io/firefly/internal/config"
	"github.com/kaleido-io/firefly/internal/log"
	"github.com/kaleido-io/firefly/mocks/databasemocks"
	"github.com/kaleido-io/firefly/mocks/datamocks"
	"github.com/kaleido-io/firefly/pkg/database"
	"github.com/kaleido-io/firefly/pkg/fftypes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

func TestE2EDispatch(t *testing.T) {
	log.SetLevel("debug")

	mdi := &databasemocks.Plugin{}
	mdm := &datamocks.Manager{}
	mdi.On("GetOffset", mock.Anything, fftypes.OffsetTypeBatch, fftypes.SystemNamespace, msgBatchOffsetName).Return(nil, nil).Once()
	mdi.On("UpsertOffset", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	mdi.On("UpdateOffset", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	mdi.On("GetOffset", mock.Anything, fftypes.OffsetTypeBatch, fftypes.SystemNamespace, msgBatchOffsetName).Return(&fftypes.Offset{
		ID: fftypes.NewUUID(),
	}, nil)
	readyForDispatch := make(chan bool)
	waitForDispatch := make(chan *fftypes.Batch)
	handler := func(ctx context.Context, b *fftypes.Batch) error {
		_, ok := <-readyForDispatch
		if !ok {
			return nil
		}
		waitForDispatch <- b
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	bmi, _ := NewBatchManager(ctx, mdi, mdm)
	bm := bmi.(*batchManager)

	bm.RegisterDispatcher(fftypes.MessageTypeBroadcast, handler, Options{
		BatchMaxSize:   2,
		BatchTimeout:   0,
		DisposeTimeout: 120 * time.Second,
	})

	dataID1 := fftypes.NewUUID()
	dataHash := fftypes.NewRandB32()
	msg := &fftypes.Message{
		Header: fftypes.MessageHeader{
			Type:      fftypes.MessageTypeBroadcast,
			ID:        fftypes.NewUUID(),
			Namespace: "ns1",
			Author:    "0x12345",
		},
		Data: fftypes.DataRefs{
			{ID: dataID1, Hash: dataHash},
		},
	}
	data := &fftypes.Data{
		ID:   dataID1,
		Hash: dataHash,
	}
	mdm.On("GetMessageData", mock.Anything, mock.Anything, true).Return([]*fftypes.Data{data}, true, nil)
	mdi.On("GetMessages", mock.Anything, mock.Anything).Return([]*fftypes.Message{msg}, nil).Once()
	mdi.On("GetMessages", mock.Anything, mock.Anything).Return([]*fftypes.Message{}, nil)
	mdi.On("UpsertBatch", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	mdi.On("UpdateBatch", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	rag := mdi.On("RunAsGroup", mock.Anything, mock.Anything, mock.Anything).Return(nil)
	rag.RunFn = func(a mock.Arguments) {
		ctx := a.Get(0).(context.Context)
		fn := a.Get(1).(func(context.Context) error)
		fn(ctx)
	}
	mdi.On("UpdateMessages", mock.Anything, mock.MatchedBy(func(f database.Filter) bool {
		fi, err := f.Finalize()
		assert.NoError(t, err)
		assert.Equal(t, fmt.Sprintf("id IN ['%s']", msg.Header.ID.String()), fi.String())
		return true
	}), mock.Anything).Return(nil)

	err := bm.Start()
	assert.NoError(t, err)

	bm.NewMessages() <- msg.Sequence

	readyForDispatch <- true
	b := <-waitForDispatch
	assert.Equal(t, *msg.Header.ID, *b.Payload.Messages[0].Header.ID)
	assert.Equal(t, *data.ID, *b.Payload.Data[0].ID)

	// Wait until everything closes
	close(readyForDispatch)
	cancel()
	bm.WaitStop()

}

func TestInitFailNoPersistence(t *testing.T) {
	_, err := NewBatchManager(context.Background(), nil, nil)
	assert.Error(t, err)
}

func TestInitRestoreExistingOffset(t *testing.T) {
	mdi := &databasemocks.Plugin{}
	mdm := &datamocks.Manager{}
	mdi.On("GetOffset", mock.Anything, fftypes.OffsetTypeBatch, fftypes.SystemNamespace, msgBatchOffsetName).Return(&fftypes.Offset{
		Type:      fftypes.OffsetTypeBatch,
		Namespace: fftypes.SystemNamespace,
		Name:      msgBatchOffsetName,
		Current:   12345,
	}, nil)
	bm, err := NewBatchManager(context.Background(), mdi, mdm)
	assert.NoError(t, err)
	defer bm.Close()
	err = bm.Start()
	assert.NoError(t, err)
	assert.Equal(t, int64(12345), bm.(*batchManager).offset)
}

func TestInitFailCannotRestoreOffset(t *testing.T) {
	mdi := &databasemocks.Plugin{}
	mdm := &datamocks.Manager{}
	mdi.On("GetOffset", mock.Anything, fftypes.OffsetTypeBatch, fftypes.SystemNamespace, msgBatchOffsetName).Return(nil, fmt.Errorf("pop"))
	bm, err := NewBatchManager(context.Background(), mdi, mdm)
	assert.NoError(t, err)
	defer bm.Close()
	bm.(*batchManager).retry.MaximumDelay = 1 * time.Microsecond
	err = bm.Start()
	assert.Regexp(t, "pop", err)
}

func TestInitFailCannotCreateOffset(t *testing.T) {
	mdi := &databasemocks.Plugin{}
	mdm := &datamocks.Manager{}
	mdi.On("GetOffset", mock.Anything, fftypes.OffsetTypeBatch, fftypes.SystemNamespace, msgBatchOffsetName).Return(nil, nil).Once()
	mdi.On("UpsertOffset", mock.Anything, mock.Anything, mock.Anything).Return(fmt.Errorf("pop"))
	mdi.On("GetOffset", mock.Anything, fftypes.OffsetTypeBatch, fftypes.SystemNamespace, msgBatchOffsetName).Return(nil, fmt.Errorf("pop"))
	bm, err := NewBatchManager(context.Background(), mdi, mdm)
	assert.NoError(t, err)
	defer bm.Close()
	bm.(*batchManager).retry.MaximumDelay = 1 * time.Microsecond
	err = bm.Start()
	assert.Regexp(t, "pop", err)
}

func TestGetInvalidBatchTypeMsg(t *testing.T) {

	mdi := &databasemocks.Plugin{}
	mdm := &datamocks.Manager{}
	mdi.On("GetOffset", mock.Anything, fftypes.OffsetTypeBatch, fftypes.SystemNamespace, msgBatchOffsetName).Return(&fftypes.Offset{
		Current: 12345,
	}, nil)
	bm, _ := NewBatchManager(context.Background(), mdi, mdm)
	defer bm.Close()
	msg := &fftypes.Message{Header: fftypes.MessageHeader{}}
	err := bm.(*batchManager).dispatchMessage(nil, msg)
	assert.Regexp(t, "FF10126", err)
}

func TestMessageSequencerCancelledContext(t *testing.T) {
	mdi := &databasemocks.Plugin{}
	mdm := &datamocks.Manager{}
	mdi.On("GetMessages", mock.Anything, mock.Anything, mock.Anything).Return(nil, fmt.Errorf("pop"))
	bm, _ := NewBatchManager(context.Background(), mdi, mdm)
	defer bm.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	bm.(*batchManager).ctx = ctx
	bm.(*batchManager).messageSequencer()
	assert.Equal(t, 1, len(mdi.Calls))
}

func TestMessageSequencerMissingMessageData(t *testing.T) {
	mdi := &databasemocks.Plugin{}
	mdm := &datamocks.Manager{}
	bm, _ := NewBatchManager(context.Background(), mdi, mdm)

	dataID := fftypes.NewUUID()
	gmMock := mdi.On("GetMessages", mock.Anything, mock.Anything, mock.Anything).Return([]*fftypes.Message{
		{
			Header: fftypes.MessageHeader{
				ID:        fftypes.NewUUID(),
				Namespace: "ns1",
			},
			Data: []*fftypes.DataRef{
				{ID: dataID},
			}},
	}, nil)
	gmMock.RunFn = func(a mock.Arguments) {
		bm.Close() // so we only go round once
	}
	mdm.On("GetMessageData", mock.Anything, mock.Anything, true).Return(nil, false, nil)

	bm.(*batchManager).messageSequencer()
	mdi.AssertExpectations(t)
	mdm.AssertExpectations(t)
}

func TestMessageSequencerDispatchFail(t *testing.T) {
	mdi := &databasemocks.Plugin{}
	mdm := &datamocks.Manager{}
	bm, _ := NewBatchManager(context.Background(), mdi, mdm)

	dataID := fftypes.NewUUID()
	gmMock := mdi.On("GetMessages", mock.Anything, mock.Anything, mock.Anything).Return([]*fftypes.Message{
		{
			Header: fftypes.MessageHeader{
				ID:        fftypes.NewUUID(),
				Type:      fftypes.MessageTypePrivate,
				Namespace: "ns1",
			},
			Data: []*fftypes.DataRef{
				{ID: dataID},
			}},
	}, nil)
	gmMock.RunFn = func(a mock.Arguments) {
		bm.Close() // so we only go round once
	}
	mdm.On("GetMessageData", mock.Anything, mock.Anything, true).Return([]*fftypes.Data{{ID: dataID}}, true, nil)

	bm.(*batchManager).messageSequencer()
	mdi.AssertExpectations(t)
	mdm.AssertExpectations(t)
}

func TestMessageSequencerUpdateMessagesClosed(t *testing.T) {
	mdi := &databasemocks.Plugin{}
	mdm := &datamocks.Manager{}
	bm, _ := NewBatchManager(context.Background(), mdi, mdm)
	bm.RegisterDispatcher(fftypes.MessageTypeBroadcast, func(c context.Context, b *fftypes.Batch) error {
		return nil
	}, Options{BatchMaxSize: 1, DisposeTimeout: 0})

	dataID := fftypes.NewUUID()
	gmMock := mdi.On("GetMessages", mock.Anything, mock.Anything, mock.Anything).Return([]*fftypes.Message{
		{
			Header: fftypes.MessageHeader{
				ID:        fftypes.NewUUID(),
				Type:      fftypes.MessageTypeBroadcast,
				Namespace: "ns1",
			},
			Data: []*fftypes.DataRef{
				{ID: dataID},
			}},
	}, nil)
	gmMock.RunFn = func(a mock.Arguments) {
		bm.Close() // so we only go round once
	}
	mdm.On("GetMessageData", mock.Anything, mock.Anything, true).Return([]*fftypes.Data{{ID: dataID}}, true, nil)
	mdi.On("UpsertBatch", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
	mdi.On("UpdateMessages", mock.Anything, mock.Anything, mock.Anything).Return(fmt.Errorf("fizzle"))
	rag := mdi.On("RunAsGroup", mock.Anything, mock.Anything, mock.Anything).Return(fmt.Errorf("pop"))
	rag.RunFn = func(a mock.Arguments) {
		ctx := a.Get(0).(context.Context)
		fn := a.Get(1).(func(context.Context) error)
		assert.Regexp(t, "fizzle", fn(ctx).Error())
	}

	bm.(*batchManager).messageSequencer()
	mdi.AssertExpectations(t)
	mdm.AssertExpectations(t)
}

func TestWaitForPollTimeout(t *testing.T) {
	mdi := &databasemocks.Plugin{}
	mdm := &datamocks.Manager{}
	bm, _ := NewBatchManager(context.Background(), mdi, mdm)
	bm.(*batchManager).messagePollTimeout = 1 * time.Microsecond
	bm.(*batchManager).waitForShoulderTapOrPollTimeout()
}

func TestWaitConsumesMessagesAndDoesNotBlock(t *testing.T) {
	config.Reset()
	mdi := &databasemocks.Plugin{}
	mdm := &datamocks.Manager{}
	bm, _ := NewBatchManager(context.Background(), mdi, mdm)
	go bm.(*batchManager).newEventNotifications()
	for i := 0; i < int(bm.(*batchManager).readPageSize); i++ {
		bm.NewMessages() <- 12345
	}
	// And should generate a shoulder tap
	<-bm.(*batchManager).shoulderTap
	bm.Close()
}

func TestAssembleMessageDataNilData(t *testing.T) {
	mdi := &databasemocks.Plugin{}
	mdm := &datamocks.Manager{}
	bm, _ := NewBatchManager(context.Background(), mdi, mdm)
	bm.Close()
	mdm.On("GetMessageData", mock.Anything, mock.Anything, true).Return(nil, false, nil)
	_, err := bm.(*batchManager).assembleMessageData(&fftypes.Message{
		Header: fftypes.MessageHeader{
			ID: fftypes.NewUUID(),
		},
		Data: fftypes.DataRefs{{ID: nil}},
	})
	assert.Regexp(t, "FF10133", err)
}

func TestAssembleMessageDataClosed(t *testing.T) {
	mdi := &databasemocks.Plugin{}
	mdm := &datamocks.Manager{}
	bm, _ := NewBatchManager(context.Background(), mdi, mdm)
	bm.(*batchManager).retry.MaximumDelay = 1 * time.Microsecond
	mdi.On("UpdateOffset", mock.Anything, mock.Anything, mock.Anything).Return(fmt.Errorf("pop"))
	err := bm.(*batchManager).updateOffset(false, 10)
	assert.EqualError(t, err, "pop")
}

func TestGetMessageDataFail(t *testing.T) {
	mdi := &databasemocks.Plugin{}
	mdm := &datamocks.Manager{}
	bm, _ := NewBatchManager(context.Background(), mdi, mdm)
	mdm.On("GetMessageData", mock.Anything, mock.Anything, true).Return(nil, false, fmt.Errorf("pop"))
	bm.Close()
	_, err := bm.(*batchManager).assembleMessageData(&fftypes.Message{
		Header: fftypes.MessageHeader{
			ID: fftypes.NewUUID(),
		},
		Data: fftypes.DataRefs{
			{ID: fftypes.NewUUID(), Hash: fftypes.NewRandB32()},
		},
	})
	assert.EqualError(t, err, "pop")
}

func TestGetMessageNotFound(t *testing.T) {
	mdi := &databasemocks.Plugin{}
	mdm := &datamocks.Manager{}
	bm, _ := NewBatchManager(context.Background(), mdi, mdm)
	mdm.On("GetMessageData", mock.Anything, mock.Anything, true).Return(nil, false, nil)
	bm.Close()
	_, err := bm.(*batchManager).assembleMessageData(&fftypes.Message{
		Header: fftypes.MessageHeader{
			ID: fftypes.NewUUID(),
		},
		Data: fftypes.DataRefs{
			{ID: fftypes.NewUUID(), Hash: fftypes.NewRandB32()},
		},
	})
	assert.Regexp(t, "FF10133", err)
}

func TestWaitForShoulderTap(t *testing.T) {
	mdi := &databasemocks.Plugin{}
	mdm := &datamocks.Manager{}
	bm, _ := NewBatchManager(context.Background(), mdi, mdm)
	bm.(*batchManager).shoulderTap <- true
	bm.(*batchManager).waitForShoulderTapOrPollTimeout()
}
