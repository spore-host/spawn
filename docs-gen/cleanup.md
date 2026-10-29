## `spawn cleanup`

Remove the shared AWS resources spore.host created (tagged spawn:managed),
in dependency order. Running instances are NEVER removed — stop or terminate
them first. A cluster placement group that still has members is reported and
skipped, since EC2 refuses to delete one — re-run once those instances are gone.

Preview what would be removed with --dry-run; otherwise cleanup prompts for
confirmation (skip with --yes) and then deletes. By default it acts only on
resources you created; --all widens to every principal in the account.

--elastic-ips releases only the Elastic IPs spawn allocated for multi-interface
EFA launches whose instance is gone (terminated, or no longer known to EC2) and
that are attached to nothing. Each one bills and counts toward the region's
Elastic IP limit. This mode touches nothing else, so running instances do not
block it. spawn list and spawn status warn about these addresses and print the
command.

--buckets removes only spawn's own spawn-staging-&lt;account&gt;-&lt;region&gt; and
spawn-results-&lt;account&gt;-&lt;region&gt; buckets, and only once they have been empty
for 7 days. It never deletes an object: a bucket holding anything at all
(objects, old versions, delete markers, an unfinished multipart upload) is
kept. S3 records no "empty since", so the first run that finds a bucket empty
tags it spawn:empty-since and a later run deletes it once that tag is 7 days
old; a run that finds it in use removes the tag, and so does a launch that
uses it. A results bucket with lifecycle rules (spawn results lifecycle
--apply) is kept, since a re-created one would not get them back. The
staging bucket is never listed (no spawn role may list it): S3 itself refuses
to delete it if it holds anything. spawn
re-creates either bucket the next time it needs it. Every call names your
account as the expected bucket owner. --dry-run writes nothing, not even the
tag. The TTL reaper can do the same on its own schedule (REAPER_EMPTY_BUCKETS).

--instance-roles deletes only the per-task spawn-instance-&lt;hash&gt; IAM roles and
instance profiles that 'spawn task run' and --iam-policy launches create, one
per distinct set of permissions, and the per-pool spawn-instance-pool-&lt;hash&gt;
ones 'spawn pool create' makes (and spawn-pool-worker-&lt;hash&gt;, their name
before v0.147.0) when the pool was never drained; a pool role also needs its
spawn:pool-run-id tag to match its name. A role goes only when it and its profile are
tagged spawn:managed=true, no instance in any enabled region (pending through
stopped) has the profile attached, the role and profile are at least 24 hours
old, and the role is unused for 24 hours. A role an enabled schedule names
(its params' iam_role) is kept until the schedule is paused, cancelled or
runs out; the schedules are read from spawn's infra account (as 'spawn
schedule' does), and if they cannot be read no role is deleted. The shared spored-instance-role is never touched. IAM is
global, so this always checks every enabled region whatever --region says, and
covers every principal's roles. The TTL reaper applies the same rule
(REAPER_INSTANCE_ROLES).

--pool-queues deletes only the SQS queues of pools nobody drained
(spawn-pool-&lt;run id&gt;) and the task specs their submitters staged in the
results bucket (staging/pool-specs/&lt;run id&gt;/). A queue goes only when it is
tagged spawn:managed=true with a spawn:pool-run-id tag matching its name, no
instance in any enabled region (pending through stopped) holds the pool's
worker profile or tag, it holds no messages, and it is at least 24 hours old.
A run's specs go once its queue is gone, no instance is the pool's, and the
newest was staged 24 hours ago. Queues made before v0.147.0 are untagged and
kept; 'spawn pool drain --run-id &lt;id&gt;' deletes one. Queues are regional, so
--region / --all-regions choose where to look; the worker check always
covers every enabled region. The TTL reaper applies the same rule
(REAPER_POOL_QUEUES).

--schedule-params deletes only schedule params objects
(spawn-schedules-&lt;region&gt;/schedules/&lt;id&gt;/params.yaml) that no schedule record
names any more: a one-time schedule whose firing failed, or an --end-after
schedule that never fired past its end, leaves its params behind once its
record expires. An object goes only when no record in the spawn-schedules
table has its id or names it (the whole table is read; if it cannot be, nothing
is deleted), it is at least 30 days old (the record's TTL margin, so a schedule
being created now is safe), and it is tagged as a schedule's params
(Component=scheduler, a matching ScheduleID, an id of the form spawn gives a
schedule). A batch queue config, which shares the bucket and key layout, is
never deleted. The schedules are read from spawn's infra account (as 'spawn
schedule' does), in every region's bucket, whatever --region says. The TTL
reaper applies the same rule (REAPER_SCHEDULE_PARAMS).

A log of everything removed is written to ~/.spawn/cleanup-&lt;timestamp&gt;.log.

```
spawn cleanup [flags]
```

**Flags:**

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--all-regions` |  | bool |  | Clean up every enabled region |
| `--all` |  | bool |  | Include resources created by other principals (default: only yours) |
| `--buckets` |  | bool |  | Delete only spawn's own staging and results buckets that have been empty for 7 days; no object is ever deleted and nothing else is touched |
| `--dry-run` |  | bool |  | Preview what would be removed without deleting anything |
| `--elastic-ips` |  | bool |  | Release only orphaned spawn launch Elastic IPs (unassociated, instance gone); nothing else is touched |
| `--instance-roles` |  | bool |  | Delete only unused per-task and per-pool spawn-instance-* (and pre-v0.147 spawn-pool-worker-*) IAM roles and instance profiles (no instance uses them, 24h old and unused); nothing else is touched |
| `--pool-queues` |  | bool |  | Delete only the SQS queues (and staged task specs) of pools nobody drained: spawn-tagged, empty, 24h old, no instance is the pool's; nothing else is touched |
| `--region` |  | string |  | AWS region (default: current region from AWS config) |
| `--schedule-params` |  | bool |  | Delete only schedule params objects no schedule record names (30 days old, tagged as a schedule's params, never a queue config); nothing else is touched |
| `--yes` | `-y` | bool |  | Skip the confirmation prompt |

