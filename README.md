Spread
======

## Convenient full-system test (task) distribution 

[Why?](#why)  
[The cascading matrix](#matrix)  
[Hello world](#hello-world)  
[Environments](#environments)  
[Variants](#variants)  
[Blacklisting and whitelisting](#blacklisting)  
[Preparing and restoring](#preparing)  
[Functions](#functions)
[Rebooting](#rebooting)  
[Timeouts](#timeouts)  
[Fast iterations with reuse](#reuse)  
[Debugging](#debugging)  
[Repeating tasks](#repeating)
[Passwords and usernames](#passwords)  
[Including, excluding, and renaming files](#including)  
[Selecting which tasks to run](#selecting)  
[Disabling unless manually selected](#manual)  
[Fetching artifacts](#artifacts)  
[LXD backend](#lxd)  
[QEMU backend](#qemu)  
[Google backend](#google)  
[Linode backend](#linode)  
[AdHoc backend](#adhoc)  
[More on parallelism](#parallelism)  
[Repacking and delta uploads](#repacking)  

<a name="why"/>

## Why?

Because our integration test machinery was unreasonably frustrating. It was
slow, very unstable, hard to make sense of the output, impossible to debug,
hard to write tests for, hard to run on multiple environments, and parallelism
was not a thing.

Spread came out as a plesant way to fix that. A few simple and concrete
concepts that are fun to play with and fix the exact piece missing in the
puzzle. It's not Jenkins, it's not Travis, it's not a library, not a language,
and it's not even specific to testing. It's a simple way to express what to run
and where, what to do before and after it runs, and how to duplicate jobs with
minor variations without copy & paste.


<a name="matrix"/>

## The cascading matrix

Each individual job in Spread has a:

 * **Project** - There's a single one of those. This is your source code
   repository or whatever the top-level thing is that you want to run tasks for.

 * **Backend** - A few lines expressing how to obtain machines for tasks to run
   on. Add as many of these as you want, as long as it's one of the supported
   backend types (bonus points for contributing a new backend type - it's rather
   easy).

 * **System** - This is the name and version of the operating system that the
   task will run on. Each backend specifies a set of systems, and optionally the
   number of machines per system (more parallelism for the impatient).
 
 * **Suite** - A couple more lines and you have a group of tasks which can
   share some settings. Defining this and all the above is done in
   `spread.yaml` or `.spread.yaml` in your project base directory.

 * **Task** - What to run, effectively. All tasks for a suite live under the same
   directory. One directory per suite, one directory per task, and `task.yaml`
   inside the latter.

 * **Variants** - All of the above is nice, but this is the reason of much
   rejoice.  Variants are a mechanism that replicates tasks with minor
   variations with no copy & paste and no trouble. See below for details.

Again, each job in spread has _a single one of each of these._ If you have two
systems and one task, there will be two jobs running in parallel on each of the
two systems. If you have two systems and three tasks, you have six jobs, three
in parallel with three. See where this is going? You can easily blacklist
specific cases too, but this is the basic idea.

Any time you want to see how your matrix looks like and all the jobs that would
run, use the `-list` option. It will show one entry per line in the format:
```
backend:system:suite/task:variant
```

<a name="hello-world"/>

## Hello world

Two tiny files and you are in business:

_$PROJECT/spread.yaml_
```
project: hello-world

backends:
    lxd:
        systems: [ubuntu-16.04]

suites:
    examples/:
        summary: Simple examples

path: /remote/path
```

_$PROJECT/examples/hello/task.yaml_
```
summary: Greet the planet
execute: |
    echo "Hello world!"
    exit 1
```

This example uses the [LXD backend](#lxd) on the local sytem and thus requires
Ubuntu 16.04 or later. If you want to distribute the tasks over to a remote
system, try the [Linode backend](#linode) with:

_$PROJECT/spread.yaml_
```
backends:
    linode:
        key: $(HOST:echo $LINODE_API_KEY)
        systems: [ubuntu-16.04]
```

Then run the example with `$ spread` from anywhere inside your project tree
for instant gratification. The echo will happen on the remote machine and
system specified, and you'll see the output locally since the task failed
(`-vv` to see the output nevertheless).

The default output for the example should look similar to this:
```
2016/06/06 09:59:34 Allocating server lxd:ubuntu-16.04...
2016/06/06 09:59:55 Waiting for LXD container spread-1-ubuntu-16-04 to have an address...
2016/06/06 09:59:59 Allocated lxd:ubuntu-16.04 (spread-1-ubuntu-16-04).
2016/06/06 09:59:59 Connecting to lxd:ubuntu-16.04 (spread-1-ubuntu-16-04)...
2016/06/06 10:00:04 Connected to lxd:ubuntu-16.04 (spread-1-ubuntu-16-04).
2016/06/06 10:00:04 Sending data to lxd:ubuntu-16.04 (spread-1-ubuntu-16-04)...
2016/06/06 10:00:05 Error executing lxd:ubuntu-16.04:examples/hello:
-----
+ echo Hello world!
Hello world!
+ exit 1
-----
2016/06/06 10:00:05 Discarding lxd:ubuntu-16.04 (spread-1-ubuntu-16-04)...
2016/06/06 10:00:06 Successful tasks: 0
2016/06/06 10:00:06 Aborted tasks: 0
2016/06/06 10:00:06 Failed tasks: 1
    - lxd:ubuntu-16.04:examples/hello
```

<a name="environments"/>

## Environments

Pretty much everything in Spread can be customized with environment variables.

_$PROJECT/examples/hello/task.yaml_
```
summary: Greet the planet
environment:
    SUBJECT: world 
    GREETING: Hello $SUBJECT!
execute: |
    echo "$GREETING"
    exit 1
```

The values defined for those variables are evaluated at the remote system and
may contain references to other variables as well as commands using the typical
`$(...)` shell syntax. As a special case, executing such commands locally at
the host running Spread is also possible via the syntax `$(HOST:...)`. This is
handy to feed local details such as API keys out of files or local environment
variables as was done on the Linode example.

Common variables and defaults are possible by defining them in the suite
or earlier:

_$PROJECT/spread.yaml_
```
(...)

suites:
    examples/:
        summary: Simple examples
        environment:
            SUBJECT: sanity
```

The cascading happens in the following order:

 * _Project => Backend => System => Suite => Task_

All of these can have an equivalent environment field and their variables will
be ordered accordingly on executed scripts.


<a name="variants"/>

## Variants

The cascading logic explained is nice, but a great deal of the convenience
offered by Spread comes from _variants._

If you understood how the environment cascading takes place, watch this:

_$PROJECT/spread.yaml_
```
(...)

suites:
    examples/:
        summary: Simple examples
        environment:
            SUBJECT/foo: sanity
            SUBJECT/bar: lunacy
```

Now every task under this suite will run twice<sup>1</sup> - once for each
variant key defined.

Then, let's redefine the examples/hello task like this:

_$PROJECT/examples/hello/task.yaml_
```
summary: Greet the planet
environment:
    GREETING: Hello
    GREETING/bar: Goodbye
    SUBJECT/baz: world
execute: |
    echo "$GREETING $SUBJECT!"
    exit 1
```

This task under that suite will spawn _three_ independent jobs, producing the
following outputs:

 * Variant "foo": _Hello sanity!_
 * Variant "bar": _Goodbye lunacy!_
 * Variant "baz": _Hello world!_

Some key takeaways here:

 * Each variant key produces a single job per task.
 * It's okay to declare the same variable with and without a variant suffix.
   The bare one becomes the default.  
 * The variant key suffix may be comma-separated for multiple definitions at
   once (`SUBJECT/foo,bar`).

<sup>1</sup> Actually, times two. It's an N-dimensional matrix.


<a name="blacklisting"/>

## Blacklisting and whitelisting

The described cascading and multiplication semantics of that matrix offers
plenty of comfort for reproducing the same tasks with minor or major
variations, but the real world is full of edge cases. Often things will be
mostly smooth except for that one case that doesn't quite make sense on such
and such situations.

For fine tuning, Spread has a convenient mechanism for blacklisting or
whitelisting particular cases across most axis of the matrix.

For example, let's avoid lunacy altogether by blacklisting the _bar_ variant
out of the particular task:
```
variants:
    - -bar
```
Alternatively, let's reach the same effect by explicitly stating which variants
to use for the task (no `-` or `+` prefix):
```
variants:
    - foo
    - baz
```
Finally, we can also append another key to current set of variants without
replacing the existing ones:
```
variants:
    - +buz
```
We've been talking about tasks, but this same field works at the project,
backend, suite, and task levels. This is also not just about variants either.
The following fields may be defined with equivalent add/remove or replace
semantics:

 * `backends: [...]` (suite and task)
 * `systems: [...]` (suite and task)
 * `variants: [...]` (project, backend, suite, and task)

So what if you don't want to run a specific task or whole suite on ubuntu-14.04?
Just add this to the task or suite body:
```
systems: [-ubuntu-14.04]
```

Cascading also takes place for these settings - each level can
add/remove/replace what the previous level defined, again with the ordering:

 * _Project => Backend => System => Suite => Task_

<a name="preparing"/>

## Preparing and restoring

A similar group of tasks will often depend on a similar setup of the system.
Instead of copying & pasting logic, suites can define scripts for tasks under
them to execute before running, and also scripts that will restore the system
to its original state so that follow up logic will find a (hopefuly? :)
unmodified base:

_$PROJECT/spread.yaml_
```
(...)

suites:
    examples/:
        summary: Simple examples
        prepare: |
            echo Preparing...
        restore: |
            echo Restoring...
```

The prepare script is called once before any of the tasks inside the suite are
run, and the restore script is called once after all of the tasks inside the
suite finish running.

Note that the restore script is called even if the prepare or execute scripts
failed _at any point while running,_ and it's supposed to do the right job of
cleaning up the system even then for follow up logic to find a pristine state.
If the restore script itself fails to execute, the whole system is considered
broken and follow up jobs will be aborted. If the restore script does a bad job
silently, you may instead lose your sleep over curious issues.

By now you may already be getting used to this, but the `prepare` and `restore`
fields are not in fact exclusive of suites. They are available at the project,
backend, suite, and task levels. In addition to those, the project, backend, and
suite levels may also hold `prepare-each` and `restore-each` fields, which are
run before and after _each task_ executes.

Assuming two tasks available under one suite, one task under another suite, and
no failures, this is the ordering of execution:

```
project prepare
    backend1 prepare
        suite1 prepare
            project prepare-each
                backend1 prepare-each
                    suite1 prepare-each
                        task1 prepare; task1 execute; task1 restore
                    suite1 restore-each
                backend1 restore-each
            project restore-each
            project prepare-each
                backend1 prepare-each
                    suite1 prepare-each
                        task2 prepare; task2 execute; task2 restore
                    suite restore-each
                backend1 restore-each
            project restore-each
        suite1 restore
        suite2 prepare
            project prepare-each
                backend1 prepare-each
                    suite2 prepare-each
                        task3 prepare; task3 execute; task3 restore
                    suite2 restore-each
                backend2 restore-each
            project restore-each
        suite2 restore
    backend1 restore
project restore
```

Typically only a few of those script slots will be used.

In addition to preparing and restoring scripts, `debug` and `debug-each`
scripts may also be defined in the same places. These are only run when other
scripts fail, and their purpose is to display further information which might
be helpful when trying to understand what went wrong.


<a name="functions"/>

## Functions

A few helper functions are available for scripts to use:

 * _REBOOT_ - Reboot the system. See [below](#rebooting) for details.
 * _MATCH_ - Run `grep -q -e` on stdin. Without match, print error including content.
 * _NOMATCH_ - Assert no match on stdin.  If match found, print error including content.
 * _ERROR_ - Fail script with provided error message only instead of script trace.
 * _FATAL_ - Similar to ERROR, but prevents retries. Specific to [adhoc backend](#adhoc).
 * _ADDRESS_ - Set allocated system address. Specific to [adhoc backend](#adhoc).


<a name="rebooting"/>

## Rebooting

Scripts can reboot the system at any point by simply running the REBOOT
function at the exact point the reboot should happen. The system will
then reboot and the same script will be re-executed with the
`$SPREAD_REBOOT` environment variable set to the number of times the
script has rebooted the system.

_$PROJECT/examples/hello/task.yaml_
```
execute: |
    if [ $SPREAD_REBOOT = 0 ]; then
        echo "Before reboot"
        REBOOT
    fi
    echo "After reboot"
```

Alternatively the REBOOT function may also be called with a single
parameter which will be used as the value of `$SPREAD_REBOOT` after
the system reboots, instead of the count.

<a name="timeouts"/>

## Timeouts

Every 5 minutes a warning will be issued including the operation output since
the last warning. If the operation does not finish within 15 minutes, it is
killed and considered an error per the usual rules of whatever is being run.
For example, a killed task is considered failed, but a killed restore script
will render the whole system broken (see [Preparing and restoring](#preparing)).

These timings may be tweaked at the project, backend, suite, and task level,
by defining the `warn-timeout` and `kill-timeout` fields with a value such as
`30s`, `1m30s`, `10m`, or `1.5h`. A value of `-1` means disable the timeout
altogether.


<a name="reuse"/>

## Fast iterations with reuse

For fast iterations during development or debugging, it's best to keep the
servers around so they're not allocated and discarded on every run. To do
that just provide the `-reuse` flag. On any successful allocation the server
details are immediately written down for tracking. Then, just provide `-reuse`
again on follow up runs and servers already alive will be used whenever
possible, and new ones will be allocated and also tracked down if necessary to
perform follow up operations.

Without the `-resend` flag, the project files previously sent are also left
alone and reused on the next run. That said, the `spread.yaml` and `task.yaml`
content considered is actually the one in the local machine, so any updates to
those will always be taken in account on re-runs.

Once you're done with the servers, throw them away with `-discard`. Reused
systems will remain running for as long as desired by default, which may run
the pool out of machines. With [Linode](#linode) you may define the
`halt-timeout` option to allow Spread itself to shutdown those systems and use
them, without destroying the data.

The obvious caveat when reusing machines like this is that failing restore
scripts or bogus ones may leave the server in a bad state which affects the
next run improperly. In such cases the restore scripts should be fixed to be
correct and more resilient.


<a name="debugging"/>

## Debugging

Debugging such remote tasking systems is generally quite boring, and Spread
offers a good hand to make the problem less painful. Just add `-debug` to
whatever set of options is in use and it will stop and open a shell at the
exact point you get a failure, with the exact same environment as the script
had.  Exit the shell and the process continues, until the next failure, no
matter which backend or system it was in.

Similarly, `-shell`, `-shell-before`, and `-shell-after` allow running an
interactive shell instead of, before, and after the original task script,
respectively, for every job that was selected to run. You'll most probably want
to filter down what is being run when using that mode, to avoid having a
troubling sequence of shells opened.

If you'd prefer to debug by logging in from an independent ssh session, the
`-abend` option will abruptly stop the execution on failures, without running
any of the restore scripts. You'll probably want to pair that with the `-reuse`
option so the server is not discarded, and after you're done with the debugging
in this style it may be necessary to do a run with the `-restore` flag, to
clean up the state left behind by the task.

Besides manual debugging through those flags, it's often handy to have more
details displayed once something does break. Next to
(preparing and restoring)[#preparing] scripts, Spread supports
specifying `debug` scripts that are run in trace mode and have their output
reported when a failure happens:

_$PROJECT/examples/hello/task.yaml_
```
execute: |
    echo "Something went wrong."
    exit 1
debug: |
    dmesg | tail
```

In a similar way to prepare and restore scripts, these can also be defined
as a `debug-each` script at the project, backend, and suite levels, so they
are aggregated and repeated for every task under them.


<a name="ordering">

## Ordering tasks

The order of tasks on every run is random by default, so that it becomes visible
when the correctness of some tasks depends on unspecified side effects of
prior tasks.

When breakages related to ordering occur, Spread can attempt to reproduce the
ordering used via the `-seed` parameter. On every run, the required seed to
reproduce the order utilized will be logged in the output. Note that when
several workers are being used, they will steal pending work from a common
queue based on timing, which means the order may not be exactly the same.

In some cases, it may also be useful to explicitly prioritize some tasks. For
example, if there are two workers and one long task, it's best if that known
long task starts first, so that the workers can share more of the load. If
the long task comes last the two workers will share all the smaller tasks,
then one worker will pick the long task, and the other worker will stop since
there's nothing else to do. The outcome is a longer total run time.

To define the priority of a task, suite, system, or backend, simply specify
the priority field in the desired context:
```
priority: 100
```

The larger the priority, the earlier it will be scheduled. The default
priority is zero, and negative priorities are supported too.


<a name="repeating"/>

## Repeating tasks

Reproducing an error may be a very boring experience, and Spread has a way to
simplify that process by reexecuting the tasks as many times as desired until
the task fails.

To do that there is an option `-repeat` which receives an integer indicating 
the number of reexecutions to do, being 0 the default value.


<a name="passwords">

## Passwords and usernames

To keep things simple and convenient, Spread prepares systems to connect over SSH
as the root user using a single password for all systems. Unless explicitly defined
via the `-pass` command line option, the password will be random and different on
each run.

Some of the supported backends may be unable to provide an image with the correct
password in place, or with the correct SSH configuration for root to connect. In
those cases, the system "username" and "password" fields may be used to tell
Spread how to perform the SSH connection:

_$PROJECT/spread.yaml_
```
backends:
    qemu:
        systems:
            - debian-sid:
                password: mypassword
            - ubuntu-16.04:
                username: ubuntu
                password: ubuntu
```

If the password field is defined without a username, it specifies the password
for root to connect over SSH.  If both username and password are provided,
the credentials will be used to connect to the system, and password-less sudo
must be available for the provided user.

In all cases the end result is the same: a system that executes scripts as root.


<a name="including"/>

## Including, excluding, and renaming files

The following fields define what is pushed to the remote servers after
each server is allocated and where that content is placed:

_$PROJECT/spread.yaml_
```
(...)

path: /remote/path

include:
    - src/*
exclude:
    - src/*.o

rename:
    - s,path/one,path/two,
```

The `path` option must be provided to define the base directory where the
content will live under, while `include` defaults to a single entry with `*`
which causes everything inside the project directory to be sent over.

The remote tree will usually look the same as the local one, but that
may be changed using the `rename` field. It takes a list of regular
expressions that act on the full relative name of each entry, and also
on the target of symbolic links. To avoid touching the target of symbolic
links append the `S` modifier as a suffix of the expression.

Note that Spread will still expect tasks to live in the same directories as
they do locally, so these directories cannot be moved.


<a name="selecting"/>

## Selecting which tasks to run

Often times you'll want to iterate over a single task or a few of these, or
a given suite, or perhaps select a specific backend to run on instead of
doing them all.

For that you can pass additional arguments indicating what to run:
```
$ spread my-suite/task-one my-suite/task-two
```

These arguments are matched against the Spread job name which uniquely
identifies it, looking like this:

    1. lxd:ubuntu-16.04:mysuite/task-one:variant-a
    2. lxd:ubuntu-16.04:mysuite/task-two:variant-b

The provided parameter must match one of the name components exactly,
optionally making use of the `...` wildcard for that. As an exception, the task
name may be matched partially as long as the slash is present as a prefix or
suffix. Matching multiple components at once is also possible separating them
with a colon; they don't have to be consecutive as long as the ordering is
correct.

For example, assuming the two jobs above, these parameters would all match
at least one of them:

  * _lxd_
  * _lxd:mysuite/_
  * _ubuntu-16.04_
  * _ubuntu-..._
  * _mysuite/_
  * _/task-one_
  * _/task..._
  * _mysu...one_
  * _lxd:ubuntu-16.04:variant-a_

The `-list` option is useful to see what jobs would be selected by a given
filter without actually running them.


<a name="manual"/>

## Disabling unless manually selected

It may be useful to have a task written down as part of the suite without it
being run all the time together with the usual tasks.  For that, just add a
`manual: true` field, and it will only be run when explicitly selected. This is
equivalent to disabing the task, except it may still be run when manually
selected.

For example:

_$PROJECT/examples/manually-run/task.yaml_
```
summary: This task only runs manually.

manual: true

...
```

The logic for explicit selection is the following: if the provided
arguments match any non-manual tasks at all, the manual tasks are not
run, even if they match the arguments.

Besides tasks, the same logic works for backends, systems, and suites.
Just add the `manual` field to their definition and they will only run
when explicitly selected, following the same logic described above: if
the provided arguments match any non-manual suite, matching manual
suites won't run, and so on.

Note that it's fine to have manual and non-manual tasks inside a manual
suite, and so forth. Play with `-list` to get a clear idea of what is
selected to run.


<a name="artifacts"/>

## Fetching residual artifacts

Content generated by tasks may easily be retrieved after the task completes
by registering the desired content under the `artifacts` field:

_$PROJECT/examples/hello/task.yaml_
```
summary: Generate some useful content.

artifacts:
    - some/file
    - some/dir/

...
```

The provided directory or file paths are relative to the task directory, and
they are only considered when Spread is run with the `-artifacts` flag pointing
to the target directory where content will be downloaded into.

For example, consider the following command:
```
$ spread -artifacts=./artifacts lxd:ubuntu-16.04:mysuite/task-one:variant-a
```

Assuming the given task has residual content registered, the directory
`./artifacts/lxd:ubuntu-16.04:mysuite/task-one:variant-a` would be created to
hold it after the job is executed.

Residual content is fetched whether the job finishes successfully or not,
and even if some of the provided paths are missing.


<a name="lxd"/>

## LXD backend

The LXD backend depends on the [LXD](http://www.ubuntu.com/cloud/lxd) container
hypervisor available on Ubuntu 16.04 or later, and allows you to run tasks
using the local system alone.

Setup LXD there with:
```
sudo apt update
sudo apt install lxd
sudo lxd init
```

Then, make sure your local user has access to the `lxc` client tool. If you
can run `lxc list` without errors, you're good to go. If not, you'll probably
have to logout and login again, or manually change your group with:
```
$ newgrp lxd
```

Then, setting up the backend in your project file is as trivial as:
```
backends:
    lxd:
        systems:
            - ubuntu-16.04
```

System names are mapped into LXD images the following way:

  * _ubuntu-16.04 => ubuntu:16.04_
  * _debian-sid => images:debian/sid/amd64_
  * _fedora-8 => images:fedora/8/amd64_
  * _etc_

Alternatively they may also be provided explicitly as:
```
backends:
    lxd:
        systems:
            - ubuntu-16.04:
                image: ubuntu:16.04.1
```

That's it. Have fun with your self-contained multi-system task runner.


<a name="qemu"/>

## QEMU backend

The QEMU backend depends on the [QEMU](http://www.qemu.org) emulator
available from various sources and allows you to run tasks using the
local system alone even if those tasks depend on low-level features
not avaliable under LXD.

Setting up the QEMU backend looks similar to:

_$PROJECT/spread.yaml_
```
backends:
    qemu:
        systems:
            - ubuntu-16.04:
                username: ubuntu
                password: ubuntu
```

For this example to work, a QEMU image must be made available under
`~/.spread/qemu/ubuntu-16.04.img`, and when run this image must open
an SSH daemon on port 22 using the provided credentials.

During the initial setup, spread will enable root access over SSH, and
will set its password to the current global password in use for the
running session as usual for every other backend (random by default,
see the `-pass` command line option).

The QEMU backend is run with the `-nographic` option by default. This
may be changed with `export SPREAD_QEMU_GUI=1`.

Note that at the moment QEMU is run via the `kvm` script, which enables
the KVM performance optimizations for the local architecture. This will
not work for other architectures, though. This problem may be easily
addressed in the future when use cases show up.

As a hint if you are using Ubuntu, here is an easy way to get a suitable
QEMU image:
```
sudo apt install qemu-kvm autopkgtest
adt-buildvm-ubuntu-cloud
```
When done move the downloaded image into the location described above.


<a name="google"/>

## Google backend

The Google backend is easy to setup and use, and allows distributing
your tasks to remote infrastructure in Google Compute Engine (GCE).

_$PROJECT/spread.yaml_
```
(...)

backends:
    google:
        key: $(HOST:echo $GOOGLE_JSON_FILENAME)
	location: yourproject/southamerica-east1-a
        systems:
            - ubuntu-16.04

	    # Extended syntax:
	    - another-system:
	        image: some-other-image
		workers: 3
```

With these settings the Google backend in Spread will pick credentials from
the JSON file pointed to in `$GOOGLE_JSON_FILENAME` environment variable
(we don't want that content inside `spread.yaml` itself). If no key is
explicitly provided, Spread will attempt to use the "application default"
credentials as traditional in the Google platform. You can set those up by
using either a service account:
```
$ gcloud auth application-default activate-service-account --key-file=$GOOGLE_JSON_FILENAME
```
or your own credentials:
```
$ gcloud auth application-default login
```
Service accounts are best as they can be further constrained and not be
associated with your overall authenticated access. Do not ship your own
credentials to remote systems.

Images are located by first attempting to match the provided value exactly
against the image name, and then some processing is done to verify if an
image with the individual tokens in its description exists. Images are
first searched for in the project itself, and then if the prefix is a
recognized name for which a public image project exists (e.g. `ubuntu-*`
is searched for in the `ubuntu-os-cloud` project too). An explicit image
project may also be requested by prefixing the image name with a project
name, as in "ubuntu-os-cloud/ubuntu-16.04-64".

When these machines terminate running, they will be removed. If anything
happens that prevents the immediate removal, they will remain in the account
and need to be removed by hand.

For long term use, a dedicated project in the Google Cloud Platform is
recommended to prevent automated manipulation of important machines.


<a name="linode"/>

## Linode backend

The Linode backend is very simple to setup and use as well, and allows
distributing your tasks over into remote infrastructure runing in
Linode's data centers.

_$PROJECT/spread.yaml_
```
(...)

backends:
    linode:
        key: $(HOST:echo $LINODE_API_KEY)
        systems:
            - ubuntu-16.04
```

With these settings the Linode backend in Spread will pick the API key from
the local `$LINODE_API_KEY` environment variable (we don't want that in
`spread.yaml`), and look for a powered-off server available on that user
account that. When it finds one, it creates a brand new configuration and
disk set to run the tasks. That means you can even reuse existing servers to
run the tasks, if you wish. When discarding the server, assuming no `-reuse` or
`-debug`, it will power off the server and remove the created configuration
and disks, leaving it ready for the next run.

The root disk is built out of a [Linode-supported distribution][linode-distros]
or a [custom image][linode-images] available in the user account. The system
name is mapped into an image or distribution label the following way:

  * _ubuntu-16.04 => Ubuntu 16.04 LTS_
  * _debian-8 => Debian 8_
  * _arch-2015-08 => Arch Linux 2015.08_
  * _etc_

Images have user-defined labels, so they're also searched for using the Spread
system name itself.

Alternatively, the extended system syntax may be used to define these details:
```
(...)

backends:
    linode:
        key: (...)
	systems:
	    - ubuntu-16.04:
	        image: Ubuntu 16.04
	        kernel: GRUB 2
```

The `image` value is matched case-insensitively as a prefix of one of the
[Linode-supported distributions][linode-distros] or a [custom
image][linode-images] available in the user account. The `kernel` value is
similarly matched against the available kernels.

Both fields are optional. Image defaults to the behavior based on system name
described above, and the kernel defaults to the latest recommended Linode
kernel.

[linode-distros]: https://www.linode.com/distributions
[linode-images]: https://www.linode.com/docs/platform/linode-images
[linode-grub2]: https://www.linode.com/docs/tools-reference/custom-kernels-distros/run-a-distribution-supplied-kernel-with-kvm

[Reused systems](#reuse) will remain running for as long as desired by default,
which may run the pool out of machines. Define the `halt-timeout` option to allow
Spread itself to shutdown those systems and use them, without destroying the data:

_$PROJECT/spread.yaml_
```
backends:
    linode:
        key: (...)
	halt-timeout: 6h
	systems:
	    - ubuntu-16.04
```

The Linode backend can also allocate systems dynamically. For that, just define
these two fields specifying which plan you'd like to use for the new machines,
and which datacenter to allocate them on:
```
backends:
    linode:
        key: (...)
	plan: 4GB
	location: newark
```

When these machines terminate running, they will be removed. If anything
happens that prevents the immediate removal, they will remain in the account
and then be reused by follow up runs and removed when done, effectively garbage
collecting what's left behind. System reuse works as explained above too.

Note that in Linode you can create additional users inside your own account
that have limited access to a selection of servers only, and with limited
permissions on them. You should use this even if your account is entirely
dedicated to Spread, because it allows you to constrain what the key in use
is allowed to do on your account. Note that you'll need to login with the
sub-user to obtain the proper key.

Some links to make your life easier:

  * [Users and permissions](https://manager.linode.com/user)
  * [API keys](https://manager.linode.com/profile/api)


<a name="adhoc"/>

## AdHoc backend

The AdHoc backend allows scripting the procedure for allocating and deallocating
systems directly in the body of the backend:

_$PROJECT/spread.yaml_
```
backends:
    adhoc:
    	allocate: |
            echo "Allocating $SPREAD_SYSTEM..."
            ADDRESS disposable.machine.address:22
        discard:
            echo "Discarding $SPREAD_SYSTEM..."
        systems:
            - ubuntu-16.04
```

The AdHoc scripts have the following custom commands available:

  * _ADDRESS addr[:port]_ - Inform SSH address of machine allocated.
  * _ERROR message_ - Exit with error message. Operation may be retried.
  * _FATAL message_ - Exit with fatal message. Operation won't be retried.
  
A failing script (non-zero exit) is equivalent to calling ERROR, but rather
than displaying a nice message, the whole script trace and output will
be shown.

The following environment variables are available for the scripts to do their job:

  * _SPREAD_BACKEND_ - Name of current backend.
  * _SPREAD_SYSTEM_ - Name of the system being allocated.
  * _SPREAD_PASSWORD_ - Password root will use to connect to the allocated system.
    Not available if the system has a custom username or password defined.
  * _SPREAD_SYSTEM_USERNAME_ - Username Spread will connect as for initial system setup.
  * _SPREAD_SYSTEM_PASSWORD_ - Password Spread will connect as for initial system setup.
  * _SPREAD_SYSTEM_ADDRESS_ - Address of the allocated system. Only available for discard.

The system allocated by the allocate script must return a system that Spread can
connect to over SSH. The system must be either setup to be accessible as root
using the session password (random or specified with -pass), or be accessible
with the username and password details specified under the system name
(see [passwords and usernames](#passwords)).

Note that the system returned by adhoc, although it can point to anything
accessible over SSH, is supposed to be a disposable system oriented towards
running the specified tasks only. It's atypical and dangerous for Spread to
be run against important systems, as it will fiddle with their configuration.


<a name="parallelism"/>

## More on parallelism

The `systems` entry under each backend contains a list of systems that will be
allocated on that backend for running tasks concurrently. 

Consider these settings:

_$PROJECT/spread.yaml_
```
(...)

backends:
    linode:
        systems:
            - ubuntu-14.04
            - ubuntu-16.04:
                workers: 2
```

This will cause three different machines to be allocated for running tasks
concurrently: one running Ubuntu 14.04 and two 16.04.

Systems share a single job pool generated out of the variable matrix, and will
run through it observing the constraints specified. For example, if there is a
backend with one ubuntu-16.04 and one ubuntu-16.10 system, and there's one
suite with 100 tasks, there will be 200 jobs and each system will run exactly
100 tasks because the 200 jobs were generated precisely so that both systems
could be exercised. On the other hand, if that same backend has instead two
ubuntu-16.04 systems, there will be only 100 jobs matching the 100 tasks, and
each system will run approximately half of them each, assuming similar task
execution duration.

Spread can also take multiple backends of the same type. In that case the
backend name will not match the backend type and thus the latter must be
provided explicitly:
```
backends:
    linode-a:
        type: linode
        (...)
    linode-b:
        type: linode
        (...)
```

This is generally not necessary, but may be useful when fine-tuning control
over the use of sets of remote machines.


<a name="repacking"/>

## Repacking and delta uploads

When working over slow networks even small uploads tend to take a bit too
long. Spread offers a general "repacking" mechanism that may be used to
transform the data being delivered in arbitrary ways, even into a delta
of content that the remote servers can obtain by themselves, for example.

The `repack` script is run with file descriptors 3 and 4 used as pipes for
the [specified](#including) project content into and out of the script,
respectively, in _tar_ format. In other words, the original specified
project content _may_ be read from file descriptor 3, and the new project
content _must_ be writen into file descriptor 4.

To illustrate, the following repack script will preserve content unchanged:

_$PROJECT/spread.yaml_
```
repack: |
    cat <&3 >&4
```

As a more complex example, the following setup explores that feature to ship a
delta from a GitHub repository, computed on top of a tag or commit reference
that is knowingly part of the history for all clients:

```
environment:
    DELTA_REF: v1.23

rename:
    - s,^,$DELTA_REF,S

exclude:
    - .git

repack: |
    trap "rm -f delta-ref.tar current.delta" EXIT
    git archive -o delta-ref.tar --format=tar --prefix=$DELTA_PREFIX $DELTA_REF
    xdelta3 -s delta-ref.tar <&3 > current.delta
    tar c current.delta >&4

prepare: |
    apt-get install xdelta3
    curl -s -o - https://codeload.github.com/myrepo/myproject/tar.gz/$DELTA_REF | gunzip > delta-ref.tar
    xdelta3 -d -s delta-ref.tar current.delta | tar x --strip-components=1
    rm -f delta-ref.tar current.delta
```

The `rename` and `exclude` settings used above ensure that the tarball that goes
into `repack` looks like the one offered by GitHub.
