package core

import (
	"database/sql"
	"fmt"
	"time"
)

const TASK_TEMPLATE_KEY = "TASK:%d"
const TASK_PREFIX_QUEUE_NAME = "task_queue_"

type TaskStatus string

const (
	TaskStatus_Doing TaskStatus = "DOING"
	TaskStatus_Done  TaskStatus = "DONE"
)

type worker struct {
	id string
}

func NewWorker() *worker {
	return &worker{
		id: ID.GenerateID(),
	}
}

func (w *worker) Start(delay time.Duration, interval time.Duration) {
	go func() {
		time.Sleep(delay)
		ticker := time.NewTicker(interval)
		for {
			select {
			case <-done:
				ticker.Stop()
				return
			case <-ticker.C:
				w.execute()
			}
		}
	}()
}

/*
* todo struct: contain taskid and bucket value that
* describe about bucket time that task must be done
 */
type todo struct {
	taskId int64
	bucket int64
}

/*
* execute: find all task from todo table and do it
 */
func (w *worker) execute() {
	bucket := GetBucket(time.Now())

	// Get all task from database: table: todo
	result, err := DBSession().QueryContext(coreContext, "SELECT task_id, bucket FROM scheduler_todo WHERE bucket <= $1 and source = $2", bucket, Config.Server.Name)
	if err != nil {
		LoggerInstance.Error("Execute tasks fail: %v", err)
		return
	}

	todos := []todo{}
	var taskId int64
	var tBucket int64

	for result.Next() {
		err := result.Scan(&taskId, &tBucket)
		if err != nil {
			LoggerInstance.Error("Get task fail: %v", err)
			return
		}

		todos = append(todos, todo{
			taskId: taskId,
			bucket: tBucket,
		})
	}

	if len(todos) == 0 {
		return
	}

	for _, todo := range todos {
		// Block this task by redis or lwt in database: use distributed log
		taskKey := fmt.Sprintf(TASK_TEMPLATE_KEY, todo.taskId)
		result, err := CacheClient().SetNX(coreContext, taskKey, string(TaskStatus_Doing), time.Duration(Config.Scheduler.TaskDoingExpiration)*time.Second).Result()
		if err != nil {
			coreContext.LogInfo("Set %s fail: %v", taskKey, err)
			continue
		} else if !result {
			coreContext.LogInfo("Key %s existed", taskKey)
			continue
		}
		// Process data
		LoggerInstance.Debug("Execute task: %s", todo.taskId)
		w.process(taskKey, todo.bucket, todo.taskId)
	}
}

func (w *worker) process(taskKey string, bucket int64, id int64) {
	// Remove blocker
	defer func() {
		// Remove block this and return
		if result, err := CacheClient().Del(coreContext, taskKey).Result(); err != nil {
			LoggerInstance.Error("Cannot delete key: %s, error = %v", taskKey, err)
		} else if result != 1 {
			LoggerInstance.Error("Delete key: %s, return differ than 1 key is deleted = %d", taskKey, result)
		}
	}()

	var t task
	// Get task detail from database in table: tasks
	row := DBSession().QueryRowContext(coreContext, "SELECT id, queue_name, data, done, loop_index, loop_count, next, interval FROM scheduler_tasks WHERE id = $1", id)
	err := row.Scan(&t.ID, &t.QueueName, &t.Data, &t.Done, &t.LoopIndex, &t.LoopCount, &t.Next, &t.Interval)
	if err != nil {
		LoggerInstance.Error("Get task fail: %v", err)
		return
	}
	LoggerInstance.Debug("Execute task: id = %d, name = %s", t.ID, t.QueueName)

	if t.Done {
		// Delete task in table: todo
		if _, err := DBSession().ExecContext(coreContext, "DELETE FROM scheduler_todo WHERE task_id = $1", id); err != nil {
			LoggerInstance.Error("Cannot delete todo task: %d", id)
		}
		return
	}

	// Start run this task: use rabbitmqt
	session, err := MessageQueue().CreateSimpleSession(QueueConfig{
		ExchangeName: BLANK,
		QueueName:    fmt.Sprintf("%s%s", TASK_PREFIX_QUEUE_NAME, t.QueueName),
		RouteKey:     BLANK,
		Kind:         MESSAGE_QUEUE_KIND_DIRECT,
		Durable:      false,
		AutoDelete:   false,
		Exclusive:    false,
		NoWait:       false,
		Args:         nil,
	})

	now := time.Now()
	if err != nil {
		LoggerInstance.Error("Fail to run task: %v: %s", t, err.Error())
		DBSession().ExecContext(coreContext, "INSERT INTO scheduler_done(bucket, task_id, operation_time, status) VALUES ($1, $2, $3, $4)", bucket, t.ID, now.Format(time.RFC3339), TASK_FAIL)
	} else {
		// Do task
		defer session.CloseSession()
		err = session.publish(t.Data)
		if err != nil {
			LoggerInstance.Error("Cannot run task: %v, err = %s", t, err.Error())
			DBSession().ExecContext(coreContext, "INSERT INTO scheduler_done(bucket, task_id, operation_time, status) VALUES ($1, $2, $3, $4)", bucket, t.ID, now.Format(time.RFC3339), TASK_FAIL)
		} else {
			_, err := DBSession().ExecContext(coreContext, "INSERT INTO scheduler_done(bucket, task_id, operation_time, status) VALUES ($1, $2, $3, $4)", bucket, t.ID, now.Format(time.RFC3339), TASK_DONE)
			if err != nil {
				fmt.Println(err.Error())
			}
		}

	}

	t.LoopIndex++
	if t.LoopIndex < t.LoopCount {
		t.Next = t.Next + t.Interval /* calculate next */
		next := time.Unix(t.Next, 0)
		newBucket := GetBucket(next)
		// Update new task in table: todo, task (time of next task)
		tx, err := DBSession().BeginTx(coreContext, &sql.TxOptions{})
		if err != nil {
			LoggerInstance.Error("Start transaction fail: %v")
		}
		defer tx.Rollback()
		// Delete old todo
		if _, err := tx.ExecContext(coreContext, "DELETE FROM scheduler_todo WHERE task_id = $1 AND bucket = $2", id, bucket); err != nil {
			LoggerInstance.Error("Fail to delete task in todo: id = %s, bucket %d", id, bucket)
		}

		// Insert new record in todo task
		if _, err := tx.ExecContext(coreContext, "INSERT INTO scheduler_todo(task_id, bucket, next_time, source) VALUES ($1, $2, $3, $4);", id, newBucket, next.Format(time.RFC3339), Config.Server.Name); err != nil {
			LoggerInstance.Error("Update todo task fail: id = %s, bucket = %d, err = %s", id, newBucket, err.Error())
		}

		// Update in task
		if _, err := tx.ExecContext(coreContext, "UPDATE scheduler_tasks SET next = $1, loop_index = $2 WHERE id = $3", t.Next, t.LoopIndex, t.ID); err != nil {
			LoggerInstance.Error("Update task fail: %v", t)
		}

		if err := tx.Commit(); err != nil {
			if err != sql.ErrTxDone {
				LoggerInstance.Error("Commit transaction fail: task = %v, %s", t, err.Error())
			}
		}

	} else {
		tx, err := DBSession().BeginTx(coreContext, &sql.TxOptions{})
		if err != nil {
			LoggerInstance.Error("Start transaction fail: %v")
		}
		defer tx.Rollback()

		// Delete old todo
		if _, err := tx.ExecContext(coreContext, "DELETE FROM scheduler_todo WHERE task_id = $1 AND bucket = $2", id, bucket); err != nil {
			LoggerInstance.Error("Fail to delete task in todo: id = %s, bucket %d", id, bucket)
		}

		// Update task in task table: is it done? => done
		if _, err := tx.ExecContext(coreContext, "UPDATE scheduler_tasks SET done = true, loop_index = $1 WHERE id = $2", t.LoopIndex, id); err != nil {
			LoggerInstance.Error("Update task in table fail: task = %v, err = %s", t, err.Error())
		}

		if err := tx.Commit(); err != nil {
			if err != sql.ErrTxDone {
				LoggerInstance.Error("Commit transaction fail: task = %v, %s", t, err.Error())
			}
		}
	}
}
