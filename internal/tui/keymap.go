package tui

// Key bindings live on the actions.
//
// This file used to hold a KeyMap struct listing twelve bindings, a Bindings()
// method returning the enabled ones, and helpRow(), which rendered a footer from
// a hardcoded string that named actions the KeyMap did not contain — e Edit and
// c Copy among them — while omitting ones it did.
//
// Three descriptions of one thing is two too many. A binding is now a field on
// the Action it invokes (see actions.go), so the key that runs something, the
// label that advertises it, and the text that explains it cannot disagree:
// there is one record and every surface reads it.
//
// The file is kept as this note rather than deleted, because "where did the
// keymap go?" is a question worth answering in the place someone will look.
