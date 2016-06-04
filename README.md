Spread - Task distribution respecting your sanity
=================================================

Why?
----

Because our integration test machinery was unreasonably frustrating. It was
slow, very unstable, hard to make sense of the output, impossible to debug,
hard to write tests for, hard to run on multiple environments, and parallelism
was not a thing.

Spread came out as a delightful way to fix that. A few simple and concrete
concepts that are fun to play with and fix the exact piece missing in the
puzzle. It's not Jenkins, it's not Travis, it's not a library, not a language,
and it's not even specific to testing. It's a simple way to express what to run
and where, what to do before and after it runs, and how to duplicate jobs with
minor variations without copy & paste.


The Matrix
----------

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
   `.spread.yaml` in your project base directory.

 * **Task** - What to run, effectively. All tasks for a suite live in the same
   directory. One directory per suite, one directory per task, and `task.yaml`
   inside it.

 * **Variants** - All of the above is nice, but this is the reason you might
   fall in love. Variants are a mechanism that replicates tasks with minor
   variations with no copy & paste and no trouble. See below for details.

Again, each job in spread has _a single one of each of these._ If you have two
systems and one task, there will be two jobs running in parallel on each of the
two systems. If you have two systems and three task, you have six jobs, three
in parallel with three. See where this is going? You can easily blacklist
specific cases too, but this is the basic idea.

Any time you want to see how your matrix looks like and all the jobs that would
run, use the `-list` option. It will show one entry per line in the format:
```
backend:system:suite/task:variant
```

Hello world
-----------

Two tiny files and you are in business:

_$PROJECT/spread.yaml_
```
project: hello-world

backend:
    linode:
        key: $(echo $LINODE_API_KEY)
        systems: [ubuntu-16.04]

suites:
    examples:
        summary: Simple examples
```

_$PROJECT/examples/hello/task.yaml_
```
summary: Greet the planet
execute:
    - echo "Hello world!"
    - exit 1
```

Run the example with `$ spread` for instant gratification. The echo will happen
on the remote machine and system specified, and you'll see the output locally
becaused the tasks have failed (`-vv` to see the output nevertheless).

Environments
------------

Pretty much everything in Spread can be customized with environment variables.

_$PROJECT/examples/hello/task.yaml_
```
summary: Greet the planet
environment:
    SUBJECT: world 
execute:
    - echo "Hello $SUBJECT!"
    - exit 1
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

_$PROJECT/examples/hello/task.yaml_
```
summary: Greet the planet
environment:
    SUBJECT: $(echo world)
execute:
    - echo "$[GREETING]"
    - exit 1
```

_$PROJECT/spread.yaml_
```
(...)

suites:
    examples:
        summary: Simple examples
        environment:
	    GREETING: "Hello $[SUBJECT]!"
```

Note that the evaluation of such variables actually happens after cascading
takes place, so it's okay to make use of variables still undefined. Errors are
caught and reported.


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
execute:
    - echo "$GREETING $SUBJECT!"
    - exit 1
```

This task under that suite will spawn _three_ independent jobs, producing the
following outputs:

 * Variant "foo": _Hello sanity!_
 * Variant "bar": _Goodbye lunacy!_
 * Variant "baz": _Hello world!_

Some key takeaways here:

 * Each variant key produces a single job, even when cascading.
 * It's okay to declare the same variable with and without a variant suffix.
   The bare one becomes the default.  
 * The variant key suffix may be comma-separated for multiple definitions at
   once (`SUBJECT/foo,bar`).

<sup>1</sup> Actually, times two. It's an N-dimensional matrix.


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
to use for the task:
```
variants:
    - foo
    - baz
```
Finally, this last case won't make much sense on this scenario, but we can also
append another key to current set of variants without replacing it:
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

So what if you don't want to run a specific task on ubuntu-14.04? Just add this
to the task:
```
systems:
    - -ubuntu-14.04
```

Cascading also takes place for these settings - each level can
add/remove/replace what the previous level defined, again with the ordering:

 * _Project => Backend => Suite => Task_

Preparing and restoring
-----------------------

A similar group of tasks will often depend on a similar setup of the system.
Instead of copying & pasting logic, suites can define scripts for tests under
them to execute before running, and also scripts that will restore the system
to its original state so that follow up logic will find a (hopefuly? :)
unmodified base:

_$PROJECT/spread.yaml_
```
(...)

suites:
    examples:
        summary: Simple examples
        prepare:
            - echo Preparing...
        restore:
            - echo Restoring...
```

The prepare script is called once before any of the tests inside the suite are
run, and the restore script is called once after all of the tests inside the
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
````

Sending content
---------------

_(write me)_

Debugging
---------

_(write me)_

More on parallelism
-------------------

The `systems` entry under each backend contains a list of systems that will be
allocated on that backend for running tasks.

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

This will cause four different machines to be allocated for running tests
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
