# Safety

`zot` is fully autonomous and has **real** file-write and shell-exec access
from `--dir`. The flag changes the process working directory; it is **not a
filesystem sandbox**. Absolute paths and shell commands retain all permissions
of the zot process. Point it at a scratch directory or a disposable git checkout
you are happy for it to change - not your home directory.

## Credentials

Configured provider credentials are resolved into zot's in-memory configuration
and then removed from the process environment before the agent starts, so its
shell commands do not inherit those API keys. Other secrets already present in
the environment or readable from disk remain accessible to those commands.

## What leaves the machine

A run talks to the provider you configured and to nothing else.

## Containers

Running zot inside a container you control is the practical way to bound all of
this: the agent can only touch the volume you mounted, and `docker run` gives you
the levers (read-only root, dropped capabilities, resource limits) in one place.
