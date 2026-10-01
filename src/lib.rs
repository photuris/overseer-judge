//! Typed verdicts for overseer judgments via TypeSafe's Jev.
//!
//! `overseer-judge` asks TypeSafe's System One API (Jev) structured
//! questions about an agent's pane capture (`session`), a task file
//! (`tasklint`), or a review round (`review`), and turns the answers
//! into typed verdicts printed as JSON.

pub mod cli;
pub mod config;
pub mod jev;
pub mod review;
pub mod session;
pub mod tasklint;
