// Copyright 2019 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package cdc

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/pingcap/check"
	"github.com/pingcap/parser/model"
	"github.com/pingcap/tidb-cdc/cdc/entry"
	"github.com/pingcap/tidb-cdc/cdc/kv"
	"github.com/pingcap/tidb-cdc/cdc/mock"
	"github.com/pingcap/tidb/types"
)

// Hook up gocheck into the "go test" runner.
func Test(t *testing.T) { check.TestingT(t) }

type CollectRawTxnsSuite struct{}

var _ = check.Suite(&CollectRawTxnsSuite{})

func (cs *CollectRawTxnsSuite) TestShouldOutputTxnsInOrder(c *check.C) {
	var entries []BufferEntry
	var startTs uint64 = 1024
	var i uint64
	for i = 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			e := BufferEntry{
				KV: &kv.RawKVEntry{
					OpType: kv.OpTypePut,
					Key:    []byte(fmt.Sprintf("key-%d-%d", i, j)),
					Ts:     startTs + i,
				},
			}
			entries = append(entries, e)
		}
	}
	// Only add resolved entry for the first 2 transaction
	for i = 0; i < 2; i++ {
		e := BufferEntry{
			Resolved: &ResolvedSpan{Timestamp: startTs + i},
		}
		entries = append(entries, e)
	}

	cursor := 0
	input := func(ctx context.Context) (BufferEntry, error) {
		if cursor >= len(entries) {
			return BufferEntry{}, errors.New("End")
		}
		e := entries[cursor]
		cursor++
		return e, nil
	}

	var rawTxns []RawTxn
	output := func(ctx context.Context, txn RawTxn) error {
		rawTxns = append(rawTxns, txn)
		return nil
	}

	ctx := context.Background()
	err := collectRawTxns(ctx, input, output)
	c.Assert(err, check.ErrorMatches, "End")

	c.Assert(rawTxns, check.HasLen, 2)
	c.Assert(rawTxns[0].ts, check.Equals, startTs)
	for i, e := range rawTxns[0].entries {
		c.Assert(e.Ts, check.Equals, startTs)
		c.Assert(string(e.Key), check.Equals, fmt.Sprintf("key-0-%d", i))
	}
	c.Assert(rawTxns[1].ts, check.Equals, startTs+1)
	for i, e := range rawTxns[1].entries {
		c.Assert(e.Ts, check.Equals, startTs+1)
		c.Assert(string(e.Key), check.Equals, fmt.Sprintf("key-1-%d", i))
	}
}

type mountTxnsSuite struct{}

var _ = check.Suite(&mountTxnsSuite{})

func setUpPullerAndSchema(c *check.C, sqls ...string) (*mock.MockTiDB, *Schema) {
	puller, err := mock.NewMockPuller(c)
	c.Assert(err, check.IsNil)
	var jobs []*model.Job

	for _, sql := range sqls {
		rawEntries := puller.MustExec(sql)
		for _, raw := range rawEntries {
			e, err := entry.Unmarshal(raw)
			c.Assert(err, check.IsNil)
			switch e := e.(type) {
			case *entry.DDLJobHistoryKVEntry:
				jobs = append(jobs, e.Job)
			}
		}
	}
	c.Assert(len(jobs), check.Equals, len(sqls))
	schema, err := NewSchema(jobs, false)
	c.Assert(err, check.IsNil)
	err = schema.handlePreviousDDLJobIfNeed(jobs[len(jobs)-1].BinlogInfo.SchemaVersion)
	c.Assert(err, check.IsNil)
	return puller, schema
}

func (cs *mountTxnsSuite) TestInsertPkNotHandle(c *check.C) {
	puller, schema := setUpPullerAndSchema(c, "create database testDB", "create table testDB.test1(id varchar(255) primary key, a int, index ci (a))")
	mounter, err := NewTxnMounter(schema, time.UTC)
	c.Assert(err, check.IsNil)

	rawKV := puller.MustExec("insert into testDB.test1 values('ttt',6)")
	txn, err := mounter.Mount(RawTxn{
		ts:      rawKV[0].Ts,
		entries: rawKV,
	})
	c.Assert(err, check.IsNil)
	cs.assertTableTxnEquals(c, txn, &Txn{
		Ts: rawKV[0].Ts,
		DMLs: []*DML{
			{
				Database: "testDB",
				Table:    "test1",
				Tp:       DeleteDMLType,
				Values: map[string]types.Datum{
					"id": types.NewBytesDatum([]byte("ttt")),
				},
			},
			{
				Database: "testDB",
				Table:    "test1",
				Tp:       InsertDMLType,
				Values: map[string]types.Datum{
					"id": types.NewBytesDatum([]byte("ttt")),
					"a":  types.NewIntDatum(6),
				},
			},
		},
	})

	rawKV = puller.MustExec("update testDB.test1 set id = 'vvv' where a = 6")
	txn, err = mounter.Mount(RawTxn{
		ts:      rawKV[0].Ts,
		entries: rawKV,
	})
	c.Assert(err, check.IsNil)
	cs.assertTableTxnEquals(c, txn, &Txn{
		Ts: rawKV[0].Ts,
		DMLs: []*DML{
			{
				Database: "testDB",
				Table:    "test1",
				Tp:       DeleteDMLType,
				Values: map[string]types.Datum{
					"id": types.NewBytesDatum([]byte("vvv")),
				},
			},
			{
				Database: "testDB",
				Table:    "test1",
				Tp:       DeleteDMLType,
				Values: map[string]types.Datum{
					"id": types.NewBytesDatum([]byte("ttt")),
				},
			},
			{
				Database: "testDB",
				Table:    "test1",
				Tp:       InsertDMLType,
				Values: map[string]types.Datum{
					"id": types.NewBytesDatum([]byte("vvv")),
					"a":  types.NewIntDatum(6),
				},
			},
		},
	})

	rawKV = puller.MustExec("delete from testDB.test1 where a = 6")
	txn, err = mounter.Mount(RawTxn{
		ts:      rawKV[0].Ts,
		entries: rawKV,
	})
	c.Assert(err, check.IsNil)
	cs.assertTableTxnEquals(c, txn, &Txn{
		Ts: rawKV[0].Ts,
		DMLs: []*DML{
			{
				Database: "testDB",
				Table:    "test1",
				Tp:       DeleteDMLType,
				Values: map[string]types.Datum{
					"id": types.NewBytesDatum([]byte("vvv")),
				},
			},
		},
	})
}

func (cs *mountTxnsSuite) TestInsertPkIsHandle(c *check.C) {
	puller, schema := setUpPullerAndSchema(c, "create database testDB", "create table testDB.test1(id int primary key, a int unique key)")
	mounter, err := NewTxnMounter(schema, time.UTC)
	c.Assert(err, check.IsNil)

	rawKV := puller.MustExec("insert into testDB.test1 values(777,888)")
	txn, err := mounter.Mount(RawTxn{
		ts:      rawKV[0].Ts,
		entries: rawKV,
	})
	c.Assert(err, check.IsNil)
	cs.assertTableTxnEquals(c, txn, &Txn{
		Ts: rawKV[0].Ts,
		DMLs: []*DML{
			{
				Database: "testDB",
				Table:    "test1",
				Tp:       DeleteDMLType,
				Values: map[string]types.Datum{
					"a": types.NewIntDatum(888),
				},
			},
			{
				Database: "testDB",
				Table:    "test1",
				Tp:       InsertDMLType,
				Values: map[string]types.Datum{
					"id": types.NewIntDatum(777),
					"a":  types.NewIntDatum(888),
				},
			},
		},
	})

	rawKV = puller.MustExec("update testDB.test1 set id = 999 where a = 888")
	txn, err = mounter.Mount(RawTxn{
		ts:      rawKV[0].Ts,
		entries: rawKV,
	})
	c.Assert(err, check.IsNil)
	cs.assertTableTxnEquals(c, txn, &Txn{
		Ts: rawKV[0].Ts,
		DMLs: []*DML{
			{
				Database: "testDB",
				Table:    "test1",
				Tp:       DeleteDMLType,
				Values: map[string]types.Datum{
					"a": types.NewIntDatum(888),
				},
			},
			{
				Database: "testDB",
				Table:    "test1",
				Tp:       DeleteDMLType,
				Values: map[string]types.Datum{
					"id": types.NewIntDatum(777),
				},
			},
			{
				Database: "testDB",
				Table:    "test1",
				Tp:       InsertDMLType,
				Values: map[string]types.Datum{
					"id": types.NewIntDatum(999),
					"a":  types.NewIntDatum(888),
				},
			},
		},
	})

	rawKV = puller.MustExec("delete from testDB.test1 where id = 999")
	txn, err = mounter.Mount(RawTxn{
		ts:      rawKV[0].Ts,
		entries: rawKV,
	})
	c.Assert(err, check.IsNil)
	cs.assertTableTxnEquals(c, txn, &Txn{
		Ts: rawKV[0].Ts,
		DMLs: []*DML{
			{
				Database: "testDB",
				Table:    "test1",
				Tp:       DeleteDMLType,
				Values: map[string]types.Datum{
					"id": types.NewIntDatum(999),
				},
			},
			{
				Database: "testDB",
				Table:    "test1",
				Tp:       DeleteDMLType,
				Values: map[string]types.Datum{
					"a": types.NewIntDatum(888),
				},
			},
		},
	})
}

func (cs *mountTxnsSuite) TestDDL(c *check.C) {
	puller, schema := setUpPullerAndSchema(c, "create database testDB", "create table testDB.test1(id varchar(255) primary key, a int, index ci (a))")
	mounter, err := NewTxnMounter(schema, time.UTC)
	c.Assert(err, check.IsNil)
	rawKV := puller.MustExec("alter table testDB.test1 add b int null")
	txn, err := mounter.Mount(RawTxn{
		ts:      rawKV[0].Ts,
		entries: rawKV,
	})
	c.Assert(err, check.IsNil)
	c.Assert(txn, check.DeepEquals, &Txn{
		DDL: &DDL{
			Database: "testDB",
			Table:    "test1",
			SQL:      "alter table testDB.test1 add b int null",
			Type:     model.ActionAddColumn,
		},
		Ts: rawKV[0].Ts,
	})

	// test insert null value
	rawKV = puller.MustExec("insert into testDB.test1(id,a) values('ttt',6)")
	txn, err = mounter.Mount(RawTxn{
		ts:      rawKV[0].Ts,
		entries: rawKV,
	})
	c.Assert(err, check.IsNil)
	cs.assertTableTxnEquals(c, txn, &Txn{
		Ts: rawKV[0].Ts,
		DMLs: []*DML{
			{
				Database: "testDB",
				Table:    "test1",
				Tp:       DeleteDMLType,
				Values: map[string]types.Datum{
					"id": types.NewBytesDatum([]byte("ttt")),
				},
			},
			{
				Database: "testDB",
				Table:    "test1",
				Tp:       InsertDMLType,
				Values: map[string]types.Datum{
					"id": types.NewBytesDatum([]byte("ttt")),
					"a":  types.NewIntDatum(6),
				},
			},
		},
	})

	rawKV = puller.MustExec("insert into testDB.test1(id,a,b) values('kkk',6,7)")
	txn, err = mounter.Mount(RawTxn{
		ts:      rawKV[0].Ts,
		entries: rawKV,
	})
	c.Assert(err, check.IsNil)
	cs.assertTableTxnEquals(c, txn, &Txn{
		Ts: rawKV[0].Ts,
		DMLs: []*DML{
			{
				Database: "testDB",
				Table:    "test1",
				Tp:       DeleteDMLType,
				Values: map[string]types.Datum{
					"id": types.NewBytesDatum([]byte("kkk")),
				},
			},
			{
				Database: "testDB",
				Table:    "test1",
				Tp:       InsertDMLType,
				Values: map[string]types.Datum{
					"id": types.NewBytesDatum([]byte("kkk")),
					"a":  types.NewIntDatum(6),
					"b":  types.NewIntDatum(7),
				},
			},
		},
	})
}

func (cs *mountTxnsSuite) assertTableTxnEquals(c *check.C,
	obtained, expected *Txn) {
	obtainedDMLs := obtained.DMLs
	expectedDMLs := expected.DMLs
	obtained.DMLs = nil
	expected.DMLs = nil
	c.Assert(obtained, check.DeepEquals, expected)
	assertContain := func(obtained []*DML, expected []*DML) {
		c.Assert(len(obtained), check.Equals, len(expected))
		for _, oDML := range obtained {
			match := false
			for _, eDML := range expected {
				if reflect.DeepEqual(oDML, eDML) {
					match = true
					break
				}
			}
			if !match {
				c.Errorf("obtained DML %#v isn't contained by expected DML", oDML)
			}
		}
	}
	assertContain(obtainedDMLs, expectedDMLs)

}
