package api

import (
	"reflect"
	"testing"

	"github.com/hashicorp/nomad/helper"
)

func TestCompose(t *testing.T) {
	t.Parallel()
	// Compose a task
	task := NewTask("task1", "exec").
		SetConfig("foo", "bar").
		SetMeta("foo", "bar").
		Constrain(NewConstraint("kernel.name", "=", "linux")).
		Require(&Resources{
			CPU:      helper.IntToPtr(1250),
			MemoryMB: helper.IntToPtr(1024),
			DiskMB:   helper.IntToPtr(2048),
			Networks: []*NetworkResource{
				{
					CIDR:          "0.0.0.0/0",
					MBits:         helper.IntToPtr(100),
					ReservedPorts: []Port{{"", 80}, {"", 443}},
				},
			},
		})

	// Compose a task group

	st1 := NewSpreadTarget("dc1", 80)
	st2 := NewSpreadTarget("dc2", 20)
	grp := NewTaskGroup("grp1", 2).
		Constrain(NewConstraint("kernel.name", "=", "linux")).
		AddAffinity(NewAffinity("${node.class}", "=", "large", 50)).
		AddSpread(NewSpread("${node.datacenter}", 30, []*SpreadTarget{st1, st2})).
		SetMeta("foo", "bar").
		AddTask(task)

	// Compose a job
	job := NewServiceJob("job1", "myjob", "region1", 2).
		SetMeta("foo", "bar").
		AddDatacenter("dc1").
		Constrain(NewConstraint("kernel.name", "=", "linux")).
		AddTaskGroup(grp)

	// Check that the composed result looks correct
	expect := &Job{
		Region:   helper.StringToPtr("region1"),
		ID:       helper.StringToPtr("job1"),
		Name:     helper.StringToPtr("myjob"),
		Type:     helper.StringToPtr(JobTypeService),
		Priority: helper.IntToPtr(2),
		Datacenters: []string{
			"dc1",
		},
		Meta: map[string]string{
			"foo": "bar",
		},
		Constraints: []*Constraint{
			{
				LTarget: "kernel.name",
				RTarget: "linux",
				Operand: "=",
			},
		},
		TaskGroups: []*TaskGroup{
			{
				Name:  helper.StringToPtr("grp1"),
				Count: helper.IntToPtr(2),
				Constraints: []*Constraint{
					{
						LTarget: "kernel.name",
						RTarget: "linux",
						Operand: "=",
					},
				},
				Affinities: []*Affinity{
					{
						LTarget: "${node.class}",
						RTarget: "large",
						Operand: "=",
						Weight:  50,
					},
				},
				Spreads: []*Spread{
					{
						Attribute: "${node.datacenter}",
						Weight:    helper.IntToPtr(30),
						SpreadTarget: []*SpreadTarget{
							{
								Value:   "dc1",
								Percent: 80,
							},
							{
								Value:   "dc2",
								Percent: 20,
							},
						},
					},
				},
				Tasks: []*Task{
					{
						Name:   "task1",
						Driver: "exec",
						Resources: &Resources{
							CPU:      helper.IntToPtr(1250),
							MemoryMB: helper.IntToPtr(1024),
							DiskMB:   helper.IntToPtr(2048),
							Networks: []*NetworkResource{
								{
									CIDR:  "0.0.0.0/0",
									MBits: helper.IntToPtr(100),
									ReservedPorts: []Port{
										{"", 80},
										{"", 443},
									},
								},
							},
						},
						Constraints: []*Constraint{
							{
								LTarget: "kernel.name",
								RTarget: "linux",
								Operand: "=",
							},
						},
						Config: map[string]interface{}{
							"foo": "bar",
						},
						Meta: map[string]string{
							"foo": "bar",
						},
					},
				},
				Meta: map[string]string{
					"foo": "bar",
				},
			},
		},
	}
	if !reflect.DeepEqual(job, expect) {
		t.Fatalf("expect: %#v, got: %#v", expect, job)
	}
}
