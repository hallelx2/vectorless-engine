# What Is Ownership?

_Ownership_ is a set of rules that govern how a Rust program manages memory.
All programs have to manage the way they use a computer's memory while running.
Some languages have garbage collection that regularly looks for no-longer-used
memory as the program runs; in other languages, the programmer must explicitly
allocate and free the memory. Rust uses a third approach: Memory is managed
through a system of ownership with a set of rules that the compiler checks. If
any of the rules are violated, the program won't compile. None of the features
of ownership will slow down your program while it's running.

Because ownership is a new concept for many programmers, it does take some time
to get used to. The good news is that the more experienced you become with Rust
and the rules of the ownership system, the easier you'll find it to naturally
develop code that is safe and efficient. Keep at it!

When you understand ownership, you'll have a solid foundation for understanding
the features that make Rust unique. In this chapter, you'll learn ownership by
working through some examples that focus on a very common data structure:
strings.

### The Stack and the Heap

Many programming languages don't require you to think about the stack and the
heap very often. But in a systems programming language like Rust, whether a
value is on the stack or the heap affects how the language behaves and why
you have to make certain decisions. Parts of ownership will be described in
relation to the stack and the heap later in this chapter, so here is a brief
explanation in preparation.

Both the stack and the heap are parts of memory available to your code to use
at runtime, but they are structured in different ways. The stack stores
values in the order it gets them and removes the values in the opposite
order. This is referred to as _last in, first out (LIFO)_. Think of a stack of
plates: When you add more plates, you put them on top of the pile, and when
you need a plate, you take one off the top. Adding or removing plates from
the middle or bottom wouldn't work as well! Adding data is called _pushing
onto the stack_, and removing data is called _popping off the stack_. All
data stored on the stack must have a known, fixed size. Data with an unknown
size at compile time or a size that might change must be stored on the heap
instead.

The heap is less organized: When you put data on the heap, you request a
certain amount of space. The memory allocator finds an empty spot in the heap
that is big enough, marks it as being in use, and returns a _pointer_, which
is the address of that location. This process is called _allocating on the
heap_ and is sometimes abbreviated as just _allocating_ (pushing values onto
the stack is not considered allocating). Because the pointer to the heap is a
known, fixed size, you can store the pointer on the stack, but when you want
the actual data, you must follow the pointer.

Pushing to the stack is faster than allocating on the heap because the
allocator never has to search for a place to store new data; that location is
always at the top of the stack. Comparatively, allocating space on the heap
requires more work because the allocator must first find a big enough space
to hold the data and then perform bookkeeping to prepare for the next allocation.

Accessing data in the heap is generally slower than accessing data on the
stack because you have to follow a pointer to get there. Contemporary
processors are faster if they jump around less in memory.

When your code calls a function, the values passed into the function
(including, potentially, pointers to data on the heap) and the function's
local variables get pushed onto the stack. When the function is over, those
values get popped off the stack.

Keeping track of what parts of code are using what data on the heap,
minimizing the amount of duplicate data on the heap, and cleaning up unused
data on the heap so that you don't run out of space are all problems that
ownership addresses. Once you understand ownership, you won't need to think
about the stack and the heap very often. But knowing that the main purpose of
ownership is to manage heap data can help explain why it works the way it does.

### Ownership Rules

First, let's take a look at the ownership rules. Keep these rules in mind as we
work through the examples that illustrate them:

- Each value in Rust has an _owner_.
- There can only be one owner at a time.
- When the owner goes out of scope, the value will be dropped.

### Variable Scope

Now that we're past basic Rust syntax, we won't include all the `fn main() {`
code in the examples. As a first example of ownership, we'll look at the scope
of some variables. A _scope_ is the range within a program for which an item
is valid.

The variable `s` refers to a string literal, where the value of the string is
hardcoded into the text of our program. The variable is valid from the point at
which it's declared until the end of the current scope.

In other words, there are two important points in time here:

- When `s` comes _into_ scope, it is valid.
- It remains valid until it goes _out of_ scope.

### The String Type

To illustrate the rules of ownership, we need a data type that is more complex
than those we covered in the Data Types section. The types covered previously
are of a known size, can be stored on the stack and popped off the stack when
their scope is over, and can be quickly and trivially copied to make a new,
independent instance if another part of code needs to use the same value in a
different scope. But we want to look at data that is stored on the heap and
explore how Rust knows when to clean up that data, and the `String` type is a
great example.

We've already seen string literals, where a string value is hardcoded into our
program. String literals are convenient, but they aren't suitable for every
situation. One reason is that they're immutable. Another is that not every
string value can be known when we write our code. For these situations Rust
has the `String` type. This type manages data allocated on the heap and as
such is able to store an amount of text that is unknown at compile time.

### Memory and Allocation

In the case of a string literal, we know the contents at compile time, so the
text is hardcoded directly into the final executable. This is why string
literals are fast and efficient. But these properties only come from the string
literal's immutability.

With the `String` type, in order to support a mutable, growable piece of text,
we need to allocate an amount of memory on the heap, unknown at compile time,
to hold the contents. This means:

- The memory must be requested from the memory allocator at runtime.
- We need a way of returning this memory to the allocator when we're done with
  our `String`.

In languages with a garbage collector (GC), the GC keeps track of and cleans
up memory that isn't being used anymore, and we don't need to think about it.
In most languages without a GC, it's our responsibility to identify when memory
is no longer being used and to call code to explicitly free it. Doing this
correctly has historically been a difficult programming problem.

Rust takes a different path: The memory is automatically returned once the
variable that owns it goes out of scope. When a variable goes out of scope,
Rust calls a special function called `drop`, and it's where the author of
`String` can put the code to return the memory. Rust calls `drop` automatically
at the closing curly bracket.

### Variables and Data Interacting with Move

Multiple variables can interact with the same data in different ways in Rust.
A `String` is made up of three parts: a pointer to the memory that holds the
contents of the string, a length, and a capacity. This group of data is stored
on the stack. On the right is the memory on the heap that holds the contents.

When we assign `s1` to `s2`, the `String` data is copied, meaning we copy the
pointer, the length, and the capacity that are on the stack. We do not copy the
data on the heap that the pointer refers to.

To ensure memory safety, after the line `let s2 = s1;`, Rust considers `s1` as
no longer valid. Therefore, Rust doesn't need to free anything when `s1` goes
out of scope. Instead of being called a shallow copy, it's known as a _move_.

There's a design choice that's implied by this: Rust will never automatically
create "deep" copies of your data. Therefore, any automatic copying can be
assumed to be inexpensive in terms of runtime performance.

### Variables and Data Interacting with Clone

If we do want to deeply copy the heap data of the `String`, not just the stack
data, we can use a common method called `clone`. When you see a call to `clone`,
you know that some arbitrary code is being executed and that code may be
expensive.

### Stack-Only Data: Copy

Types such as integers that have a known size at compile time are stored
entirely on the stack, so copies of the actual values are quick to make. Rust
has a special annotation called the `Copy` trait that we can place on types
that are stored on the stack. If a type implements the `Copy` trait, variables
that use it do not move, but rather are trivially copied.

Some of the types that implement `Copy`:

- All the integer types, such as `u32`.
- The Boolean type, `bool`, with values `true` and `false`.
- All the floating-point types, such as `f64`.
- The character type, `char`.
- Tuples, if they only contain types that also implement `Copy`.

### Ownership and Functions

The mechanics of passing a value to a function are similar to those when
assigning a value to a variable. Passing a variable to a function will move or
copy, just as assignment does.

### Return Values and Scope

Returning values can also transfer ownership. The ownership of a variable
follows the same pattern every time: Assigning a value to another variable
moves it. When a variable that includes data on the heap goes out of scope,
the value will be cleaned up by `drop` unless ownership of the data has been
moved to another variable.

Rust does let us return multiple values using a tuple. But this is too much
ceremony and a lot of work for a concept that should be common. Luckily for
us, Rust has a feature for using a value without transferring ownership:
references.
