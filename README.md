Spread
======

Convenient full-system test (task) distribution 
-----------------------------------------------

[Why?](#why)  
[The cascading matrix](#matrix)  
[Hello world](#hello-world)  
[Environments](#environments)  
[Environment interpolation](#interpolation)  
[Variants](#variants)  
[Blacklisting and whitelisting](#blacklisting)  
[Preparing and restoring](#preparing)  
[Fast iterations with reuse](#reuse)
[Debugging](#debugging)  
[Keeping servers](#keeping)  
[Including and excluding files](#including)  
[Selecting which tasks to run](#selecting)  
[LXD backend](#lxd)  
[Linode backend](#linode)  
[More on parallelism](#parallelism)  

<a name="why"/>
Why?
----

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
The cascading matrix
--------------------

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
Hello world
-----------

Two tiny files and you are in business:

_$PROJECT/spread.yaml_
```
project: hello-world

backend:
    lxd:
        systems: [ubuntu-16.04]

suites:
    examples:
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
backend:
    linode:
        key: $(echo $LINODE_API_KEY)
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
Environments
------------

Pretty much everything in Spread can be customized with environment variables.

_$PROJECT/examples/hello/task.yaml_
```
summary: Greet the planet
environment:
    SUBJECT: world 
execute: |
    echo "Hello $SUBJECT!"
    exit 1
```

And we could set a default for this same environment variable by defining it at
the suite level:

_$PROJECT/spread.yaml_
```
(...)

suites:
    examples:
        summary: Simple examples
        environment:
            SUBJECT: sanity
```

The cascading happens in the following order:

 * _Project => Backend => Suite => Task_

All of these can have an equivalent environment field.

<a name="interpolation"/>
Environment interpolation
-------------------------

In the example above, the script was delivered as-is and the environment variables were
evaluated by the remote server using the environment defined by Spread for the script
execution. As an alternative, interpolation may also happen using the environment defined
inside Spread itself by refering to variables as `$[NAME]`. This adds some convenience,
specially when playing with cascading, and ensures the values reaching the server
already hold their final value.

Even trickier cases may be handled by shelling out into the local system for obtaining
variable values or parts of them. This is done with the more typical `$(cmdline)` form.
You may have noticed this being used before, when defining the Linode API key. We don't
want such a key hardcoded on a public text file, so we can dig into the local environment
for finding it out.

For instance, an alternative spelling for the previous example might be:

_$PROJECT/spread.yaml_
```
(...)

suites:
    examples:
        summary: Simple examples
        environment:
            GREETING: "Hello $[SUBJECT]!"
```

_$PROJECT/examples/hello/task.yaml_
```
summary: Greet the planet
environment:
    SUBJECT: $(echo world)
execute: |
    echo "$[GREETING]"
    exit 1
```

Note that the evaluation of such variables actually happens after cascading
takes place, so it's okay to make use of variables still undefined. Errors are
caught and reported.


<a name="variants"/>
Variants
--------

The cascading logic explained is nice, but a great deal of the convenience
offered by Spread comes from _variants._

If you understood how the environment cascading takes place, watch this:

_$PROJECT/spread.yaml_
```
(...)

suites:
    examples:
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
Blacklisting and whitelisting
-----------------------------

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

 * _Project => Backend => Suite => Task_

<a name="preparing"/>
Preparing and restoring
-----------------------

A similar group of tasks will often depend on a similar setup of the system.
Instead of copying & pasting logic, suites can define scripts for tasks under
them to execute before running, and also scripts that will restore the system
to its original state so that follow up logic will find a (hopefuly? :)
unmodified base:

_$PROJECT/spread.yaml_
```
(...)

suites:
    examples:
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
If the restore script fails to execute, the whole system is considered broken
and follow up jobs will be aborted. If the restore script does a bad job
silently, you may lose your sleep over curious issues.

By now you may already be getting used to this, but the `prepare` and `execute`
fields are not in fact exclusive of suites. They are available at the project,
backend, suite, and task levels. Assuming two tasks available under one suite,
one task under another suite, and no failures, this is the ordering of
execution:

```
project prepare
    backend1 prepare
        suite1 prepare
            task1 prepare; task1 execute; task1 restore
            task2 prepare; task2 execute; task2 restore
        suite1 restore
        suite2 prepare
            task3 prepare; task3 execute; task3 restore
        suite2 restore
    backend1 restore
project restore
```

<a name="reuse"/>
Fast iterations with reuse
--------------------------

For fast iterations during development or debugging, it's best to keep the
servers around so they're not allocated and discarded on every run. To do
that just provide the `-keep` flag. As long as allocation worked, at the
end of the run the servers will not be discarded and Spread will inform
the exact line to reuse these servers.

Unless you use the `-resend` flag, the project files previously sent are also
left alone and reused on the next run. That said, the `spread.yaml` and
`task.yaml` content considered is actually the local one, so any updates to
those will always be taken in account on re-runs.


Debugging
---------

Debugging such remote tasking systems is generally quite boring, and Spread
offers a good hand to make the problem less painful. Just add `-debug` to
whatever set of options is in use and it stop and open a shell at the exact
point you get a failure, with the exact same environment as the script had.
Exit the shell and the process continues, until the next failure, no matter
which backend or system it was in.

A similar option is to use `-shell`. Rather than stopping on failures, this
will run the shell _instead of_ the original task scripts, for every job
that was selected to run. You'll most probably want to filter down what is
being run when using that mode, to avoid having a troubling sequence of shells
opened.

If you'd prefer to debug by logging in from an independent ssh session, the
`-abend` option will abruptly stop the execution on failures, without running
any of the restore scripts. You'll probably want to pair that with the `-keep`
option so the server is not discarded, and after you're done with the debugging
it may be necessary to do a run with the `-restore` flag, to clean up the
state left behind by the task.


<a name="keeping"/>
Keeping servers
---------------

Even if server allocation is fast, not allocating at all is even faster.
Just add `-keep` to your current set of options and servers will remain running
after workers are done with them. At the end of the process, Spread will report
how to reuse them.

Unlike the debug mode described above, this will not alter the running process
otherwise. The remote project is still removed at the end of the run and
re-sent on the next iteration as if the machine was brand new.

The obvious caveat when reusing machines like this is that failing restore
scripts or bogus ones may leave the server in a bad state which affects the
next run improperly. In such cases the restore scripts should be fixed to be
correct and more resilient.

Same as with `-debug`, after you're done iterating it's easy to get rid of the
servers by performing one last run including the `-reuse` option, but leaving
`-keep` out.

<a name="including"/>
Including and excluding files
-----------------------------

There are three fields that control what is pushed to the remote servers after
each server is allocated:

_$PROJECT/spread.yaml_
```
(...)

path: /remote/path

include:
    - src/*
exclude:
    - src/*.o
```

The `path` option must be provided, while `include` defaults to a single
entry with `*` which causes everything inside the project directory to be sent
over.  Nothing is excluded by default.

<a name="selecting"/>
Selecting which tasks to run
----------------------------

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

<a name="lxd"/>
LXD backend
-----------

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
  * _fedora-8_ => images:fedora/8/amd64_
  * _etc_

That's it. Have fun with your self-contained multi-system task runner.


<a name="linode"/>
Linode backend
--------------

The Linode backend is very simple to setup and use as well, and allows
distributing your tasks over into remote infrastructure.

_$PROJECT/spread.yaml_
```
(...)

backend:
    linode:
        key: $(echo $LINODE_API_KEY)
        systems:
            - ubuntu-16.04
```

With these settings the Linode backend in Spread will pick the API key from
the local `$LINODE_API_KEY` environment variable (we don't want that in
`spread.yaml`), and look for a powered-off server available on that user
account that. When it finds one, it creates a brand new configuration and
disk set to run the tasks. That means you can even reuse existing servers to
run the tasks, if you wish. When discarding the server, assuming no `-keep` or
`-debug`, it will power off the server and remove the created configuration
and disks, leaving it ready for the next run.

The root disk is built out a [Linode-supported distribution][linode-distros]
or a [custom image][linode-images] available in the user account. The system
name is mapped into an image or distribution label the following way:

  * _ubuntu-16.04 => Ubuntu 16.04 LTS_
  * _debian-8 => Debian 8_
  * _arch-2015-08_ => Arch Linux 2015.08_
  * _etc_

Images have user-defined labels, so they're also searched for using the Spread
system name itself.

The kernel used in the server configuration is the latest Linode kernel
available, or the [GRUB 2][grub2] special kernel if the system name ends in
`-grub`. The latter allows running custom images with the correct
distribution-provided kernel.

[linode-distros]: https://www.linode.com/distributions
[linode-images]: https://www.linode.com/docs/platform/linode-images
[linode-grub2]: https://www.linode.com/docs/tools-reference/custom-kernels-distros/run-a-distribution-supplied-kernel-with-kvm


Note that in Linode you can create additional users inside your own account
that have limited access to a selection of servers only, and with limited
permissions on them. You should use this even if your account is entirely
dedicated to Spread, because it allows you to constrain what the key in use
is allowed to do on your account. Note that you'll need to login with the
sub-user to obtain the proper key.

Some links to make your life easier:

  * [Users and permissions](https://manager.linode.com/user)
  * [API keys](https://manager.linode.com/profile/api)


<a name="parallelism"/>
More on parallelism
-------------------

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
            - ubuntu-16.04*2
            - ubuntu-16.10/foo,bar
```

This will cause four different machines to be allocated for running tasks
concurrently: one running Ubuntu 14.04, two 16.04, and one 16.10. The 16.10
machine will only run jobs for variants _foo_ and _bar_, while the others will
accept any variants (including _foo_ and _bar_).

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
