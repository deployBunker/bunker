#!/usr/bin/perl
# anchor-replace.pl OLDFILE NEWFILE FILE — replace OLDFILE's exact content with
# NEWFILE's in FILE, and REFUSE unless the anchor appears exactly once. A mutation
# that no longer matches the source must be an ABORT, never a silent no-op that
# would leave the "mutated" tree unchanged and make the arm vacuous.
#
# It prints what it found, so a mismatch is diagnosable rather than silent.
# usage: perl anchor-replace.pl OLD NEW FILE   (exit 2 when the anchor is not unique)
use strict;
use warnings;
my ($oldf, $newf, $file) = @ARGV;
die "usage: anchor-replace.pl OLD NEW FILE\n" unless defined $file;
open(my $o, "<", $oldf) or die "open old: $!";
open(my $n, "<", $newf) or die "open new: $!";
open(my $f, "<", $file) or die "open src: $!";
local $/;
my $old = <$o>;
my $new = <$n>;
my $src = <$f>;
close $o; close $n; close $f;
my $count = () = $src =~ /\Q$old\E/g;
if ($count != 1) {
  print "MUTATION ANCHOR appears $count time(s), want exactly 1 (anchor ", length($old), " bytes)\n";
  print "  fragment (first 160 bytes): ", substr($old, 0, 160), "\n";
  exit 2;
}
$src =~ s/\Q$old\E/$new/ or die "substitution failed\n";
open(my $out, ">", $file) or die "write $file: $!";
print $out $src;
close $out;
print "anchor replaced (1 occurrence, ", length($old), " bytes -> ", length($new), " bytes)\n";
