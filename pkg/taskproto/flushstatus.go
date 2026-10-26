package taskproto

// FlushStatusTag is the instance tag spored writes with the outcome of a task's
// terminal flush (#1186): "ok", "failed:<reason>" or "skipped:<reason>". It is
// the only record of the flush that outlives the instance's disk, and only for
// as long as EC2 still lists the terminated instance, about an hour.
// tasklaunch.ReadFlushStatus is its reader (#1295).
const FlushStatusTag = "spawn:flush-status"

// FlushStatusOK is the FlushStatusTag value for a flush that wrote the task's
// terminal record.
const FlushStatusOK = "ok"
